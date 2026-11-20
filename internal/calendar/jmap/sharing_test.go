package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	principals map[string]jmapPrincipal // by email (lower-case)
	// setErr, when non-nil, is returned as the Calendar/set notUpdated error.
	setErr map[string]any
	// noCalendar makes Calendar/get return an empty list.
	noCalendar bool
	patches    []map[string]any // every Calendar/set update patch received
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
			list := []any{map[string]any{"id": "cal-1", "name": "Cal", "shareWith": f.share}}
			if f.noCalendar {
				list = []any{}
			}
			resp = map[string]any{"accountId": "acc1", "state": "s", "notFound": []any{}, "list": list}
		case "Calendar/set":
			if f.setErr != nil {
				resp = map[string]any{"accountId": "acc1", "newState": "s", "notUpdated": map[string]any{"cal-1": f.setErr}}
				break
			}
			if upd, ok := args["update"].(map[string]any); ok {
				if c1, ok := upd["cal-1"].(map[string]any); ok {
					f.patches = append(f.patches, c1)
					for k, v := range c1 {
						pid, ok := strings.CutPrefix(k, "shareWith/")
						if !ok {
							continue
						}
						if v == nil {
							delete(f.share, pid)
							continue
						}
						b, _ := json.Marshal(v)
						var cr calendarRights
						_ = json.Unmarshal(b, &cr)
						f.share[pid] = &cr
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
					if p, ok := f.principals[strings.ToLower(email)]; ok {
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
	if perms, err := cl.ListCalendarPermissions(ctx, "cal-1"); err != nil || len(perms) != 1 || perms[0].Role != calendar.RoleWrite {
		t.Errorf("after update: %+v, %v; want one write grant", perms, err)
	}
	if _, err := cl.UpdateCalendarPermission(ctx, "cal-1", "ghost", calendar.Permission{Role: calendar.RoleRead}); !errors.Is(err, calendar.ErrPermissionNotFound) {
		t.Errorf("update missing = %v, want ErrPermissionNotFound", err)
	}

	if err := cl.DeleteCalendarPermission(ctx, "cal-1", "bob"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if perms, err := cl.ListCalendarPermissions(ctx, "cal-1"); err != nil || len(perms) != 0 {
		t.Errorf("after delete: %+v, %v", perms, err)
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

func TestRoleRightsTable(t *testing.T) {
	cases := []struct {
		role    calendar.PermissionRole
		allowed bool
	}{
		{calendar.RoleNone, true}, {calendar.RoleFreeBusyRead, true}, {calendar.RoleRead, true}, {calendar.RoleWrite, true},
		{calendar.RoleLimitedRead, false}, {calendar.RoleDelegate, false}, {calendar.RoleDelegatePrivate, false}, {calendar.RoleCustom, false},
	}
	for _, c := range cases {
		r, err := rightsForRole(c.role)
		if c.allowed {
			if err != nil {
				t.Errorf("%q: unexpected error %v", c.role, err)
				continue
			}
			if got := rightsToRole(r); got != c.role {
				t.Errorf("%q round-tripped to %q", c.role, got)
			}
			if r.MayShare || r.MayDelete {
				t.Errorf("%q must not grant mayShare/mayDelete: %+v", c.role, *r)
			}
			if r.MayWriteAll && !(r.MayWriteOwn && r.MayUpdatePrivate && r.MayRSVP) {
				t.Errorf("%q: mayWriteAll without implied flags: %+v", c.role, *r)
			}
		} else if !errors.Is(err, calendar.ErrRoleNotAllowed) {
			t.Errorf("%q: err = %v, want ErrRoleNotAllowed", c.role, err)
		}
	}
	w := roleToRights(calendar.RoleWrite)
	want := calendarRights{MayReadFreeBusy: true, MayReadItems: true, MayWriteAll: true, MayWriteOwn: true, MayUpdatePrivate: true, MayRSVP: true}
	if *w != want {
		t.Errorf("write rights = %+v, want %+v", *w, want)
	}
}

func TestRightsToRoleExternalGrants(t *testing.T) {
	cases := []struct {
		r    *calendarRights
		want calendar.PermissionRole
	}{
		{nil, calendar.RoleNone},
		{&calendarRights{}, calendar.RoleNone},
		{&calendarRights{MayReadFreeBusy: true}, calendar.RoleFreeBusyRead},
		{&calendarRights{MayReadFreeBusy: true, MayReadItems: true, MayRSVP: true}, calendar.RoleRead},
		{&calendarRights{MayReadItems: true, MayWriteAll: true, MayDelete: true, MayShare: true}, calendar.RoleWrite},
	}
	for _, c := range cases {
		if got := rightsToRole(c.r); got != c.want {
			t.Errorf("rightsToRole(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestJMAPSharingRefusesUnsupportedRoles(t *testing.T) {
	f := &shareFake{share: map[string]*calendarRights{"bob": {MayReadFreeBusy: true}}, principals: map[string]jmapPrincipal{
		"bob@example.com": {ID: "bob", Name: "Bob", Email: "bob@example.com"},
	}}
	cl := newShareClient(t, f)
	ctx := context.Background()
	for _, role := range []calendar.PermissionRole{calendar.RoleLimitedRead, calendar.RoleDelegate, calendar.RoleDelegatePrivate, calendar.RoleCustom} {
		if _, err := cl.CreateCalendarPermission(ctx, "cal-1", calendar.Permission{Email: "bob@example.com", Role: role}); !errors.Is(err, calendar.ErrRoleNotAllowed) {
			t.Errorf("create %q: err = %v, want ErrRoleNotAllowed", role, err)
		}
		if _, err := cl.UpdateCalendarPermission(ctx, "cal-1", "bob", calendar.Permission{Role: role}); !errors.Is(err, calendar.ErrRoleNotAllowed) {
			t.Errorf("update %q: err = %v, want ErrRoleNotAllowed", role, err)
		}
	}
	if got := f.share["bob"]; got == nil || *got != (calendarRights{MayReadFreeBusy: true}) {
		t.Errorf("refused roles mutated the backend: %+v", got)
	}
}

func TestJMAPSharingAllowedRolesAndReadBack(t *testing.T) {
	f := &shareFake{share: map[string]*calendarRights{}, principals: map[string]jmapPrincipal{
		"bob@example.com": {ID: "bob", Name: "Bob", Email: "bob@example.com"},
	}}
	cl := newShareClient(t, f)
	ctx := context.Background()
	created, err := cl.CreateCalendarPermission(ctx, "cal-1", calendar.Permission{Email: "bob@example.com", Role: calendar.RoleWrite})
	if err != nil {
		t.Fatal(err)
	}
	if created.Role != calendar.RoleWrite || len(created.AllowedRoles) != 4 {
		t.Errorf("created = %+v", created)
	}
	if f.share["bob"].MayDelete || f.share["bob"].MayShare {
		t.Errorf("write grant carries mayDelete/mayShare: %+v", *f.share["bob"])
	}
	perms, err := cl.ListCalendarPermissions(ctx, "cal-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(perms) != 1 || len(perms[0].AllowedRoles) != 4 || perms[0].Role != calendar.RoleWrite {
		t.Errorf("listed = %+v", perms)
	}
	updated, err := cl.UpdateCalendarPermission(ctx, "cal-1", "bob", calendar.Permission{Role: calendar.RoleRead})
	if err != nil || updated.Role != calendar.RoleRead || len(updated.AllowedRoles) != 4 {
		t.Errorf("updated = %+v, %v", updated, err)
	}
}

func TestSetSharePatchesOnlyTheTargetPrincipal(t *testing.T) {
	// An entry made outside the gateway (extra flags, even non-conforming) must not
	// be rewritten by a change to another grantee, and the patch must be a
	// per-principal path so concurrent writers cannot clobber each other.
	eve := calendarRights{MayReadFreeBusy: true, MayReadItems: true, MayWriteAll: true, MayShare: true, MayDelete: true}
	f := &shareFake{share: map[string]*calendarRights{"eve": &eve}, principals: map[string]jmapPrincipal{
		"bob@example.com": {ID: "bob", Name: "Bob", Email: "bob@example.com"},
	}}
	cl := newShareClient(t, f)
	ctx := context.Background()
	if _, err := cl.CreateCalendarPermission(ctx, "cal-1", calendar.Permission{Email: "bob@example.com", Role: calendar.RoleWrite}); err != nil {
		t.Fatal(err)
	}
	if got := f.share["eve"]; got == nil || *got != eve {
		t.Errorf("unrelated entry rewritten: %+v, want %+v", got, eve)
	}
	if len(f.patches) != 1 {
		t.Fatalf("patches = %v", f.patches)
	}
	for k := range f.patches[0] {
		if k != "shareWith/bob" {
			t.Errorf("patch key %q, want only shareWith/bob (no whole-map write)", k)
		}
	}
	if err := cl.DeleteCalendarPermission(ctx, "cal-1", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.share["bob"]; ok {
		t.Error("bob not revoked")
	}
	if f.patches[1]["shareWith/bob"] != nil {
		t.Errorf("revoke patch = %v, want null", f.patches[1])
	}
	if got := f.share["eve"]; got == nil || *got != eve {
		t.Errorf("unrelated entry rewritten by revoke: %+v", got)
	}
}

func TestJMAPCreateResponseMatchesGet(t *testing.T) {
	f := &shareFake{share: map[string]*calendarRights{}, principals: map[string]jmapPrincipal{
		"bob@example.com": {ID: "bob", Name: "Bob Builder", Email: "bob@example.com"},
	}}
	cl := newShareClient(t, f)
	ctx := context.Background()
	// Request with different casing and a caller-supplied name.
	created, err := cl.CreateCalendarPermission(ctx, "cal-1", calendar.Permission{Email: "BOB@Example.com", Name: "x", Role: calendar.RoleRead})
	if err != nil {
		t.Fatal(err)
	}
	got, err := cl.GetCalendarPermission(ctx, "cal-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created, got) || got.Email != "bob@example.com" || got.Name != "Bob Builder" {
		t.Errorf("create = %+v, get = %+v", created, got)
	}
	updated, err := cl.UpdateCalendarPermission(ctx, "cal-1", created.ID, calendar.Permission{Email: "zzz", Name: "zzz", Role: calendar.RoleWrite})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = cl.GetCalendarPermission(ctx, "cal-1", created.ID)
	if !reflect.DeepEqual(updated, got) || got.Email != "bob@example.com" {
		t.Errorf("update = %+v, get = %+v", updated, got)
	}
}

func TestJMAPSharingBackendRefusals(t *testing.T) {
	principals := map[string]jmapPrincipal{"bob@example.com": {ID: "bob", Name: "Bob", Email: "bob@example.com"}}
	ctx := context.Background()

	owner := &shareFake{share: map[string]*calendarRights{}, principals: principals,
		setErr: map[string]any{"type": "invalidProperties", "description": "An object cannot be shared with the principal that owns it.", "properties": []string{"shareWith"}}}
	_, err := newShareClient(t, owner).CreateCalendarPermission(ctx, "cal-1", calendar.Permission{Email: "bob@example.com", Role: calendar.RoleRead})
	if !errors.Is(err, calendar.ErrInvalidGrantee) {
		t.Errorf("owner share err = %v, want ErrInvalidGrantee", err)
	}
	if err != nil && strings.Contains(err.Error(), "owns it") {
		t.Errorf("backend text leaked into error: %v", err)
	}

	missing := &shareFake{share: map[string]*calendarRights{"bob": {}}, principals: principals, noCalendar: true}
	if _, err := newShareClient(t, missing).UpdateCalendarPermission(ctx, "cal-1", "bob", calendar.Permission{Role: calendar.RoleRead}); !errors.Is(err, calendar.ErrCalendarNotFound) {
		t.Errorf("missing calendar update err = %v, want ErrCalendarNotFound", err)
	}
	nf := &shareFake{share: map[string]*calendarRights{}, principals: principals, setErr: map[string]any{"type": "notFound"}}
	if _, err := newShareClient(t, nf).CreateCalendarPermission(ctx, "cal-1", calendar.Permission{Email: "bob@example.com", Role: calendar.RoleRead}); !errors.Is(err, calendar.ErrCalendarNotFound) {
		t.Errorf("notFound create err = %v, want ErrCalendarNotFound", err)
	}
}
