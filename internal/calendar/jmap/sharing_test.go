package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// shareSessionJSON advertises the principals capability (which the default
// sessionJSON omits) so go-jmap permits the Principal/* methods.
func shareSessionJSON(apiURL string) string {
	return `{"capabilities":{"urn:ietf:params:jmap:core":{},"urn:ietf:params:jmap:calendars":{},"urn:ietf:params:jmap:principals":{}},` +
		`"accounts":{"acc1":{"name":"u","isPersonal":true,"accountCapabilities":{}}},` +
		`"primaryAccounts":{"urn:ietf:params:jmap:calendars":"acc1","urn:ietf:params:jmap:principals":"acc1"},` +
		`"apiUrl":"` + apiURL + `","downloadUrl":"","uploadUrl":"","eventSourceUrl":"","state":"s"}`
}

// shareFake is a stateful JMAP fake for the sharing methods: it keeps a shareWith
// map for one calendar ("cal-1") and resolves principals by email, so the full
// PermissionReader/Writer round-trip runs over the real go-jmap client + wire.
type shareFake struct {
	share      map[string]*calendarRights
	principals map[string]jmapPrincipal // by email
}

func (f *shareFake) handle(w http.ResponseWriter, body map[string]any) {
	calls, _ := body["methodCalls"].([]any)
	var responses []any
	for _, c := range calls {
		call := c.([]any)
		name, _ := call[0].(string)
		args, _ := call[1].(map[string]any)
		cid, _ := call[2].(string)
		var resp map[string]any
		switch name {
		case "Calendar/get":
			resp = map[string]any{"accountId": "acc1", "state": "s", "notFound": []any{},
				"list": []any{map[string]any{"id": "cal-1", "name": "Cal", "shareWith": f.share}}}
		case "Calendar/set":
			if upd, ok := args["update"].(map[string]any); ok {
				if c1, ok := upd["cal-1"].(map[string]any); ok {
					if sw, ok := c1["shareWith"].(map[string]any); ok {
						f.share = map[string]*calendarRights{}
						for pid, r := range sw {
							b, _ := json.Marshal(r)
							var cr calendarRights
							_ = json.Unmarshal(b, &cr)
							f.share[pid] = &cr
						}
					}
				}
			}
			resp = map[string]any{"accountId": "acc1", "newState": "s2", "updated": map[string]any{"cal-1": nil}}
		case "Principal/get":
			ids, _ := args["ids"].([]any)
			var list []any
			for _, id := range ids {
				for _, p := range f.principals {
					if string(p.ID) == id {
						list = append(list, p)
					}
				}
			}
			resp = map[string]any{"accountId": "acc1", "list": list}
		case "Principal/query":
			var ids []any
			if filt, ok := args["filter"].(map[string]any); ok {
				if email, ok := filt["email"].(string); ok {
					if p, ok := f.principals[email]; ok {
						ids = append(ids, string(p.ID))
					}
				}
			}
			resp = map[string]any{"accountId": "acc1", "ids": ids}
		}
		responses = append(responses, []any{name, resp, cid})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s"})
}

func newShareClient(t *testing.T, f *shareFake) *Client {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(shareSessionJSON(srv.URL + "/jmap")))
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.handle(w, req)
	}))
	t.Cleanup(srv.Close)
	cl, err := Dial(srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return cl
}

func TestJMAPCalendarSharingCRUD(t *testing.T) {
	f := &shareFake{share: map[string]*calendarRights{}, principals: map[string]jmapPrincipal{
		"bob@example.com": {ID: "bob", Type: "individual", Name: "Bob", Email: "bob@example.com"},
	}}
	cl := newShareClient(t, f)
	ctx := context.Background()

	if perms, err := cl.ListCalendarPermissions(ctx, "cal-1"); err != nil || len(perms) != 0 {
		t.Fatalf("initial list = %v, %v", perms, err)
	}

	created, err := cl.CreateCalendarPermission(ctx, "cal-1", calendar.Permission{Email: "bob@example.com", Role: calendar.RoleRead})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != "bob" {
		t.Errorf("created id = %q, want bob (the principal id)", created.ID)
	}

	perms, err := cl.ListCalendarPermissions(ctx, "cal-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(perms) != 1 || perms[0].Email != "bob@example.com" || perms[0].Name != "Bob" || perms[0].Role != calendar.RoleRead {
		t.Fatalf("perms = %+v", perms)
	}

	if _, err := cl.GetCalendarPermission(ctx, "cal-1", "bob"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := cl.GetCalendarPermission(ctx, "cal-1", "nope"); !errors.Is(err, calendar.ErrPermissionNotFound) {
		t.Errorf("get missing = %v, want ErrPermissionNotFound", err)
	}

	if _, err := cl.UpdateCalendarPermission(ctx, "cal-1", "bob", calendar.Permission{Role: calendar.RoleWrite}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if perms, _ := cl.ListCalendarPermissions(ctx, "cal-1"); perms[0].Role != calendar.RoleWrite {
		t.Errorf("role after update = %q, want write", perms[0].Role)
	}
	if _, err := cl.UpdateCalendarPermission(ctx, "cal-1", "ghost", calendar.Permission{Role: calendar.RoleRead}); !errors.Is(err, calendar.ErrPermissionNotFound) {
		t.Errorf("update missing = %v, want ErrPermissionNotFound", err)
	}

	if err := cl.DeleteCalendarPermission(ctx, "cal-1", "bob"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if perms, _ := cl.ListCalendarPermissions(ctx, "cal-1"); len(perms) != 0 {
		t.Errorf("after delete: %+v", perms)
	}
	if err := cl.DeleteCalendarPermission(ctx, "cal-1", "bob"); !errors.Is(err, calendar.ErrPermissionNotFound) {
		t.Errorf("delete missing = %v, want ErrPermissionNotFound", err)
	}
}

func TestJMAPCreatePermissionUnknownPrincipal(t *testing.T) {
	cl := newShareClient(t, &shareFake{share: map[string]*calendarRights{}, principals: map[string]jmapPrincipal{}})
	if _, err := cl.CreateCalendarPermission(context.Background(), "cal-1", calendar.Permission{Email: "ghost@example.com", Role: calendar.RoleRead}); err == nil {
		t.Error("create with an unresolvable email = nil error, want failure")
	}
}

func TestRoleRightsRoundTrip(t *testing.T) {
	// Roles that map onto a distinct rights set and back to themselves.
	for _, role := range []calendar.PermissionRole{
		calendar.RoleNone, calendar.RoleFreeBusyRead, calendar.RoleRead,
		calendar.RoleWrite, calendar.RoleDelegatePrivate,
	} {
		if got := rightsToRole(roleToRights(role)); got != role {
			t.Errorf("role %q round-tripped to %q", role, got)
		}
	}
	// Delegate-without-private collapses to write on read-back (write and delegate
	// share the same rights bits); that lossiness is expected.
	if got := rightsToRole(roleToRights(calendar.RoleDelegate)); got != calendar.RoleWrite {
		t.Errorf("delegate read back as %q, want write (documented lossiness)", got)
	}
}
