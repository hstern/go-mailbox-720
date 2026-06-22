package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// Calendar sharing: Graph calendarPermission under
// /me/calendars/{calendar-id}/calendarPermissions (MB720-24). These handlers
// translate a Graph calendarPermission to and from the neutral calendar.Permission
// and drive the calendar backend's optional PermissionReader / PermissionWriter
// capability. A backend that does not implement it yields 501, the same posture as
// quota, message rules, and the calendar write path. Only the /me/* variants are
// served; /users/{user-id}/* fall through to the unimplemented handler.

// MeCalendarsListCalendarPermissions implements GET /me/calendars/{id}/calendarPermissions.
func (h Handler) MeCalendarsListCalendarPermissions(ctx context.Context, params api.MeCalendarsListCalendarPermissionsParams) (api.MeCalendarsListCalendarPermissionsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	pr, ok := b.(calendar.PermissionReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	perms, err := pr.ListCalendarPermissions(ctx, params.CalendarID)
	if err != nil {
		return nil, fmt.Errorf("list calendar permissions: %w", err)
	}
	value := make([]api.MicrosoftGraphCalendarPermission, 0, len(perms))
	for _, p := range perms {
		value = append(value, toGraphCalendarPermission(p))
	}
	return &api.MicrosoftGraphCalendarPermissionCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphCalendarPermissionCollectionResponse{Value: value},
	}, nil
}

// MeCalendarsGetCalendarPermissions implements GET /me/calendars/{id}/calendarPermissions/{id}.
func (h Handler) MeCalendarsGetCalendarPermissions(ctx context.Context, params api.MeCalendarsGetCalendarPermissionsParams) (api.MeCalendarsGetCalendarPermissionsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	pr, ok := b.(calendar.PermissionReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	p, err := pr.GetCalendarPermission(ctx, params.CalendarID, params.CalendarPermissionID)
	if err != nil {
		if errors.Is(err, calendar.ErrPermissionNotFound) {
			return notFound("calendar permission not found"), nil
		}
		return nil, fmt.Errorf("get calendar permission: %w", err)
	}
	return &api.MicrosoftGraphCalendarPermissionStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphCalendarPermission(p),
	}, nil
}

// MeCalendarsCreateCalendarPermissions implements POST /me/calendars/{id}/calendarPermissions —
// granting a new share.
func (h Handler) MeCalendarsCreateCalendarPermissions(ctx context.Context, req *api.MicrosoftGraphCalendarPermission, params api.MeCalendarsCreateCalendarPermissionsParams) (api.MeCalendarsCreateCalendarPermissionsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	pw, ok := b.(calendar.PermissionWriter)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	p := mergeGraphPermission(calendar.Permission{}, req)
	if p.Email == "" {
		return badRequest("calendarPermission requires emailAddress.address"), nil
	}
	created, err := pw.CreateCalendarPermission(ctx, params.CalendarID, p)
	if err != nil {
		return nil, fmt.Errorf("create calendar permission: %w", err)
	}
	return &api.MicrosoftGraphCalendarPermissionStatusCode{
		StatusCode: http.StatusCreated,
		Response:   toGraphCalendarPermission(created),
	}, nil
}

// MeCalendarsUpdateCalendarPermissions implements PATCH /me/calendars/{id}/calendarPermissions/{id}.
// PATCH is a partial update: the existing grant is read and the request's set fields
// (typically the role) are overlaid, so an omitted member is left untouched. The
// merge needs both capabilities; a backend missing either yields 501.
func (h Handler) MeCalendarsUpdateCalendarPermissions(ctx context.Context, req *api.MicrosoftGraphCalendarPermission, params api.MeCalendarsUpdateCalendarPermissionsParams) (api.MeCalendarsUpdateCalendarPermissionsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	pw, okW := b.(calendar.PermissionWriter)
	pr, okR := b.(calendar.PermissionReader)
	if !okW || !okR {
		return nil, ht.ErrNotImplemented
	}
	existing, err := pr.GetCalendarPermission(ctx, params.CalendarID, params.CalendarPermissionID)
	if err != nil {
		if errors.Is(err, calendar.ErrPermissionNotFound) {
			return notFound("calendar permission not found"), nil
		}
		return nil, fmt.Errorf("get calendar permission: %w", err)
	}
	updated, err := pw.UpdateCalendarPermission(ctx, params.CalendarID, params.CalendarPermissionID, mergeGraphPermission(existing, req))
	if err != nil {
		if errors.Is(err, calendar.ErrPermissionNotFound) {
			return notFound("calendar permission not found"), nil
		}
		return nil, fmt.Errorf("update calendar permission: %w", err)
	}
	return &api.MicrosoftGraphCalendarPermissionStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphCalendarPermission(updated),
	}, nil
}

// MeCalendarsDeleteCalendarPermissions implements DELETE /me/calendars/{id}/calendarPermissions/{id} —
// revoking a share.
func (h Handler) MeCalendarsDeleteCalendarPermissions(ctx context.Context, params api.MeCalendarsDeleteCalendarPermissionsParams) (api.MeCalendarsDeleteCalendarPermissionsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	pw, ok := b.(calendar.PermissionWriter)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	if err := pw.DeleteCalendarPermission(ctx, params.CalendarID, params.CalendarPermissionID); err != nil {
		if errors.Is(err, calendar.ErrPermissionNotFound) {
			return notFound("calendar permission not found"), nil
		}
		return nil, fmt.Errorf("delete calendar permission: %w", err)
	}
	return &api.MeCalendarsDeleteCalendarPermissionsNoContent{}, nil
}

// mergeGraphPermission overlays the set fields of a Graph calendarPermission body
// onto an existing neutral permission, implementing PATCH's partial-update semantics.
// A create passes an empty base, so the overlay reduces to "take every set field".
// The read-only ID, AllowedRoles, and IsRemovable are never taken from the body.
func mergeGraphPermission(base calendar.Permission, g *api.MicrosoftGraphCalendarPermission) calendar.Permission {
	p := base
	if ea, ok := g.EmailAddress.Get(); ok {
		if v, ok := ea.Address.Get(); ok {
			p.Email = v
		}
		if v, ok := ea.Name.Get(); ok {
			p.Name = v
		}
	}
	if r, ok := g.Role.Get(); ok {
		p.Role = calendar.PermissionRole(r)
	}
	return p
}

// toGraphCalendarPermission maps the neutral calendar.Permission onto the generated
// Graph type.
func toGraphCalendarPermission(p calendar.Permission) api.MicrosoftGraphCalendarPermission {
	g := api.MicrosoftGraphCalendarPermission{
		ID: api.NewOptString(p.ID),
		EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
			Address: api.NewOptNilString(p.Email),
			Name:    api.NewOptNilString(p.Name),
		}),
		Role:                 api.NewOptMicrosoftGraphCalendarRoleType(api.MicrosoftGraphCalendarRoleType(p.Role)),
		IsInsideOrganization: api.NewOptNilBool(p.IsInsideOrganization),
		IsRemovable:          api.NewOptNilBool(p.IsRemovable),
	}
	if len(p.AllowedRoles) > 0 {
		g.AllowedRoles = make([]api.MicrosoftGraphCalendarRoleType, 0, len(p.AllowedRoles))
		for _, r := range p.AllowedRoles {
			g.AllowedRoles = append(g.AllowedRoles, api.MicrosoftGraphCalendarRoleType(r))
		}
	}
	return g
}
