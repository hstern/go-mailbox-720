package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// permCalendarBackend is a calendar.Backend (via the embedded fakeCalendarBackend)
// that also implements PermissionReader/PermissionWriter over an in-memory grant set.
type permCalendarBackend struct {
	fakeCalendarBackend
	perms  []calendar.Permission
	nextID int
}

func (b *permCalendarBackend) ListCalendarPermissions(context.Context, string) ([]calendar.Permission, error) {
	out := make([]calendar.Permission, len(b.perms))
	copy(out, b.perms)
	return out, nil
}

func (b *permCalendarBackend) GetCalendarPermission(_ context.Context, _, id string) (calendar.Permission, error) {
	for _, p := range b.perms {
		if p.ID == id {
			return p, nil
		}
	}
	return calendar.Permission{}, calendar.ErrPermissionNotFound
}

func (b *permCalendarBackend) CreateCalendarPermission(_ context.Context, _ string, p calendar.Permission) (calendar.Permission, error) {
	b.nextID++
	p.ID = fmt.Sprintf("perm-%d", b.nextID)
	p.IsRemovable = true
	b.perms = append(b.perms, p)
	return p, nil
}

func (b *permCalendarBackend) UpdateCalendarPermission(_ context.Context, _, id string, p calendar.Permission) (calendar.Permission, error) {
	for i := range b.perms {
		if b.perms[i].ID == id {
			p.ID = id
			b.perms[i] = p
			return p, nil
		}
	}
	return calendar.Permission{}, calendar.ErrPermissionNotFound
}

func (b *permCalendarBackend) DeleteCalendarPermission(_ context.Context, _, id string) error {
	for i := range b.perms {
		if b.perms[i].ID == id {
			b.perms = append(b.perms[:i], b.perms[i+1:]...)
			return nil
		}
	}
	return calendar.ErrPermissionNotFound
}

type permProvider struct{ b calendar.Backend }

func (p permProvider) Calendar(context.Context) (calendar.Backend, error) { return p.b, nil }

func permHandler(b calendar.Backend) Handler {
	return Handler{calendar: permProvider{b: b}}
}

func grantReq(email string, role api.MicrosoftGraphCalendarRoleType) *api.MicrosoftGraphCalendarPermission {
	return &api.MicrosoftGraphCalendarPermission{
		EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{Address: api.NewOptNilString(email)}),
		Role:         api.NewOptMicrosoftGraphCalendarRoleType(role),
	}
}

func TestCalendarPermissionsCreateListGet(t *testing.T) {
	h := permHandler(&permCalendarBackend{})
	ctx := context.Background()

	res, err := h.MeCalendarsCreateCalendarPermissions(ctx, grantReq("bob@example.com", api.MicrosoftGraphCalendarRoleTypeRead), api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "cal-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created, ok := res.(*api.MicrosoftGraphCalendarPermissionStatusCode)
	if !ok || created.StatusCode != http.StatusCreated {
		t.Fatalf("create response = %T (status %v), want 201", res, statusOf(res))
	}
	id, _ := created.Response.ID.Get()
	if id == "" {
		t.Fatal("created permission has no id")
	}

	lres, err := h.MeCalendarsListCalendarPermissions(ctx, api.MeCalendarsListCalendarPermissionsParams{CalendarID: "cal-1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	coll := lres.(*api.MicrosoftGraphCalendarPermissionCollectionResponseStatusCode)
	if len(coll.Response.Value) != 1 {
		t.Fatalf("permission count = %d, want 1", len(coll.Response.Value))
	}
	got := coll.Response.Value[0]
	if ea, _ := got.EmailAddress.Get(); ea.Address.Or("") != "bob@example.com" {
		t.Errorf("email = %q, want bob@example.com", ea.Address.Or(""))
	}
	if r, _ := got.Role.Get(); r != api.MicrosoftGraphCalendarRoleTypeRead {
		t.Errorf("role = %q, want read", r)
	}

	if _, err := h.MeCalendarsGetCalendarPermissions(ctx, api.MeCalendarsGetCalendarPermissionsParams{CalendarID: "cal-1", CalendarPermissionID: id}); err != nil {
		t.Fatalf("get: %v", err)
	}
	mres, _ := h.MeCalendarsGetCalendarPermissions(ctx, api.MeCalendarsGetCalendarPermissionsParams{CalendarID: "cal-1", CalendarPermissionID: "nope"})
	if e, ok := mres.(*api.ErrorStatusCode); !ok || e.StatusCode != http.StatusNotFound {
		t.Fatalf("get missing = %T (status %v), want 404", mres, statusOf(mres))
	}
}

func TestCalendarPermissionUpdatePatchMergesRole(t *testing.T) {
	b := &permCalendarBackend{}
	h := permHandler(b)
	ctx := context.Background()
	created, _ := h.MeCalendarsCreateCalendarPermissions(ctx, grantReq("bob@example.com", api.MicrosoftGraphCalendarRoleTypeRead), api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "cal-1"})
	id, _ := created.(*api.MicrosoftGraphCalendarPermissionStatusCode).Response.ID.Get()

	// PATCH only the role; the grantee email must survive.
	patch := &api.MicrosoftGraphCalendarPermission{Role: api.NewOptMicrosoftGraphCalendarRoleType(api.MicrosoftGraphCalendarRoleTypeWrite)}
	if _, err := h.MeCalendarsUpdateCalendarPermissions(ctx, patch, api.MeCalendarsUpdateCalendarPermissionsParams{CalendarID: "cal-1", CalendarPermissionID: id}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if b.perms[0].Role != calendar.RoleWrite {
		t.Errorf("role not updated: %q", b.perms[0].Role)
	}
	if b.perms[0].Email != "bob@example.com" {
		t.Errorf("email wiped by partial PATCH: %q", b.perms[0].Email)
	}

	res, _ := h.MeCalendarsUpdateCalendarPermissions(ctx, patch, api.MeCalendarsUpdateCalendarPermissionsParams{CalendarID: "cal-1", CalendarPermissionID: "ghost"})
	if e, ok := res.(*api.ErrorStatusCode); !ok || e.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing = %T (status %v), want 404", res, statusOf(res))
	}
}

func TestCalendarPermissionDelete(t *testing.T) {
	b := &permCalendarBackend{}
	h := permHandler(b)
	ctx := context.Background()
	created, _ := h.MeCalendarsCreateCalendarPermissions(ctx, grantReq("bob@example.com", api.MicrosoftGraphCalendarRoleTypeRead), api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "cal-1"})
	id, _ := created.(*api.MicrosoftGraphCalendarPermissionStatusCode).Response.ID.Get()

	res, err := h.MeCalendarsDeleteCalendarPermissions(ctx, api.MeCalendarsDeleteCalendarPermissionsParams{CalendarID: "cal-1", CalendarPermissionID: id})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := res.(*api.MeCalendarsDeleteCalendarPermissionsNoContent); !ok {
		t.Fatalf("delete response = %T, want NoContent", res)
	}
	if len(b.perms) != 0 {
		t.Errorf("permission not removed")
	}
	mres, _ := h.MeCalendarsDeleteCalendarPermissions(ctx, api.MeCalendarsDeleteCalendarPermissionsParams{CalendarID: "cal-1", CalendarPermissionID: id})
	if e, ok := mres.(*api.ErrorStatusCode); !ok || e.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing = %T (status %v), want 404", mres, statusOf(mres))
	}
}

func TestCalendarPermissionCreateRequiresEmail(t *testing.T) {
	h := permHandler(&permCalendarBackend{})
	req := &api.MicrosoftGraphCalendarPermission{Role: api.NewOptMicrosoftGraphCalendarRoleType(api.MicrosoftGraphCalendarRoleTypeRead)}
	res, err := h.MeCalendarsCreateCalendarPermissions(context.Background(), req, api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "cal-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e, ok := res.(*api.ErrorStatusCode); !ok || e.StatusCode != http.StatusBadRequest {
		t.Fatalf("create without email = %T (status %v), want 400", res, statusOf(res))
	}
}

func TestCalendarPermissionsNotImplemented(t *testing.T) {
	// A calendar backend without the sharing capability.
	h := permHandler(&fakeCalendarBackend{})
	ctx := context.Background()
	if _, err := h.MeCalendarsListCalendarPermissions(ctx, api.MeCalendarsListCalendarPermissionsParams{CalendarID: "c"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("list err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeCalendarsGetCalendarPermissions(ctx, api.MeCalendarsGetCalendarPermissionsParams{CalendarID: "c", CalendarPermissionID: "x"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("get err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeCalendarsCreateCalendarPermissions(ctx, grantReq("x@y.z", api.MicrosoftGraphCalendarRoleTypeRead), api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "c"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("create err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeCalendarsUpdateCalendarPermissions(ctx, grantReq("x@y.z", api.MicrosoftGraphCalendarRoleTypeRead), api.MeCalendarsUpdateCalendarPermissionsParams{CalendarID: "c", CalendarPermissionID: "x"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("update err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeCalendarsDeleteCalendarPermissions(ctx, api.MeCalendarsDeleteCalendarPermissionsParams{CalendarID: "c", CalendarPermissionID: "x"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("delete err = %v, want ErrNotImplemented", err)
	}
}

// roleNormalizingBackend mimics an adapter whose read-back role differs from the
// requested one (as the JMAP tier's rights-to-role inference can), and refuses
// limitedRead like the JMAP adapter.
type roleNormalizingBackend struct{ permCalendarBackend }

func (b *roleNormalizingBackend) norm(p calendar.Permission) (calendar.Permission, error) {
	switch p.Role {
	case calendar.RoleLimitedRead:
		return p, fmt.Errorf("%w: %q", calendar.ErrRoleNotAllowed, p.Role)
	case calendar.RoleDelegatePrivate:
		p.Role = calendar.RoleWrite
	}
	return p, nil
}

func (b *roleNormalizingBackend) CreateCalendarPermission(ctx context.Context, cal string, p calendar.Permission) (calendar.Permission, error) {
	p, err := b.norm(p)
	if err != nil {
		return calendar.Permission{}, err
	}
	return b.permCalendarBackend.CreateCalendarPermission(ctx, cal, p)
}

func (b *roleNormalizingBackend) UpdateCalendarPermission(ctx context.Context, cal, id string, p calendar.Permission) (calendar.Permission, error) {
	p, err := b.norm(p)
	if err != nil {
		return calendar.Permission{}, err
	}
	return b.permCalendarBackend.UpdateCalendarPermission(ctx, cal, id, p)
}

func TestCalendarPermissionsRejectedRoleIs400(t *testing.T) {
	h := permHandler(&roleNormalizingBackend{})
	ctx := context.Background()
	res, err := h.MeCalendarsCreateCalendarPermissions(ctx, grantReq("bob@example.com", api.MicrosoftGraphCalendarRoleTypeLimitedRead), api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "c"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if e, ok := res.(*api.ErrorStatusCode); !ok || e.StatusCode != http.StatusBadRequest {
		t.Errorf("create res = %#v, want 400", res)
	}

	if _, err := h.MeCalendarsCreateCalendarPermissions(ctx, grantReq("bob@example.com", api.MicrosoftGraphCalendarRoleTypeRead), api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "c"}); err != nil {
		t.Fatal(err)
	}
	ures, err := h.MeCalendarsUpdateCalendarPermissions(ctx, grantReq("", api.MicrosoftGraphCalendarRoleTypeLimitedRead), api.MeCalendarsUpdateCalendarPermissionsParams{CalendarID: "c", CalendarPermissionID: "perm-1"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if e, ok := ures.(*api.ErrorStatusCode); !ok || e.StatusCode != http.StatusBadRequest {
		t.Errorf("update res = %#v, want 400", ures)
	}
}

func TestCalendarPermissionsResponseRoleMatchesReadBack(t *testing.T) {
	b := &roleNormalizingBackend{}
	h := permHandler(b)
	ctx := context.Background()
	res, err := h.MeCalendarsCreateCalendarPermissions(ctx, grantReq("bob@example.com", api.MicrosoftGraphCalendarRoleTypeDelegateWithPrivateEventAccess), api.MeCalendarsCreateCalendarPermissionsParams{CalendarID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	created := res.(*api.MicrosoftGraphCalendarPermissionStatusCode).Response
	got, _ := h.MeCalendarsGetCalendarPermissions(ctx, api.MeCalendarsGetCalendarPermissionsParams{CalendarID: "c", CalendarPermissionID: "perm-1"})
	read := got.(*api.MicrosoftGraphCalendarPermissionStatusCode).Response
	if created.Role != read.Role || created.Role.Value != api.MicrosoftGraphCalendarRoleTypeWrite {
		t.Errorf("create role %v vs read-back %v, want both write", created.Role, read.Role)
	}

	ures, err := h.MeCalendarsUpdateCalendarPermissions(ctx, grantReq("", api.MicrosoftGraphCalendarRoleTypeDelegateWithPrivateEventAccess), api.MeCalendarsUpdateCalendarPermissionsParams{CalendarID: "c", CalendarPermissionID: "perm-1"})
	if err != nil {
		t.Fatal(err)
	}
	updated := ures.(*api.MicrosoftGraphCalendarPermissionStatusCode).Response
	if updated.Role.Value != api.MicrosoftGraphCalendarRoleTypeWrite {
		t.Errorf("update role = %v, want write (read-back)", updated.Role)
	}
}
