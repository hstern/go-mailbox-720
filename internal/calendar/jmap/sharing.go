package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// This file implements calendar.PermissionReader / PermissionWriter on the JMAP
// calendar backend (MB720-24 chunk C): calendar sharing over JMAP Sharing
// (RFC 9670, urn:ietf:params:jmap:principals) plus the JMAP Calendars shareWith
// field. go-jmap ships no sharing types, so the Principal/get, Principal/query, and
// Calendar/set methods and the per-principal rights object are hand-rolled here
// (the same approach as the contacts adapter for RFC 9610). A Graph
// calendarPermission grant is one entry in the calendar's shareWith map, keyed by
// the grantee's Principal id; the rights vocabulary and shapes were confirmed
// against a live Stalwart server.

// principalsURI is the RFC 9670 JMAP capability URN for principals.
const principalsURI gojmap.URI = "urn:ietf:params:jmap:principals"

func init() {
	gojmap.RegisterMethod("Calendar/set", func() gojmap.MethodResponse { return &calendarSetResponse{} })
	gojmap.RegisterMethod("Principal/get", func() gojmap.MethodResponse { return &principalGetResponse{} })
	gojmap.RegisterMethod("Principal/query", func() gojmap.MethodResponse { return &principalQueryResponse{} })
}

var (
	_ calendar.PermissionReader = (*Client)(nil)
	_ calendar.PermissionWriter = (*Client)(nil)
)

// calendarRights is the JMAP Calendars per-principal rights object (the value type
// of myRights and shareWith), as confirmed against Stalwart. All eight flags are
// always present on the wire.
type calendarRights struct {
	MayReadFreeBusy  bool `json:"mayReadFreeBusy"`
	MayReadItems     bool `json:"mayReadItems"`
	MayWriteAll      bool `json:"mayWriteAll"`
	MayWriteOwn      bool `json:"mayWriteOwn"`
	MayUpdatePrivate bool `json:"mayUpdatePrivate"`
	MayRSVP          bool `json:"mayRSVP"`
	MayShare         bool `json:"mayShare"`
	MayDelete        bool `json:"mayDelete"`
}

// --- Calendar/set (used only to mutate shareWith) ---

type calendarSet struct {
	Account gojmap.ID                    `json:"accountId"`
	Update  map[gojmap.ID]map[string]any `json:"update,omitempty"`
}

func (m *calendarSet) Name() string           { return "Calendar/set" }
func (m *calendarSet) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type calendarSetResponse struct {
	Account    gojmap.ID                      `json:"accountId"`
	NewState   string                         `json:"newState"`
	Updated    map[gojmap.ID]json.RawMessage  `json:"updated"`
	NotUpdated map[gojmap.ID]*gojmap.SetError `json:"notUpdated"`
}

// --- Principal/get (RFC 9670) ---

type jmapPrincipal struct {
	ID    gojmap.ID `json:"id"`
	Type  string    `json:"type"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
}

type principalGet struct {
	Account gojmap.ID   `json:"accountId"`
	IDs     []gojmap.ID `json:"ids,omitempty"`
}

func (m *principalGet) Name() string           { return "Principal/get" }
func (m *principalGet) Requires() []gojmap.URI { return []gojmap.URI{principalsURI} }

type principalGetResponse struct {
	Account gojmap.ID        `json:"accountId"`
	List    []*jmapPrincipal `json:"list"`
}

// --- Principal/query (RFC 9670) ---

type principalQuery struct {
	Account gojmap.ID        `json:"accountId"`
	Filter  *principalFilter `json:"filter,omitempty"`
}

type principalFilter struct {
	Email string `json:"email,omitempty"`
}

func (m *principalQuery) Name() string           { return "Principal/query" }
func (m *principalQuery) Requires() []gojmap.URI { return []gojmap.URI{principalsURI} }

type principalQueryResponse struct {
	Account gojmap.ID   `json:"accountId"`
	IDs     []gojmap.ID `json:"ids"`
}

// principalAccountID resolves the account serving the principals capability,
// falling back to the calendar account (Stalwart serves both from one account).
func (cl *Client) principalAccountID() gojmap.ID {
	if cl.c.Session != nil {
		if id := cl.c.Session.PrimaryAccounts[principalsURI]; id != "" {
			return id
		}
	}
	return cl.accountID
}

// ListCalendarPermissions reads the calendar's shareWith map and resolves each
// grantee principal to its email/name. The grant id is the principal id.
func (cl *Client) ListCalendarPermissions(ctx context.Context, calendarID string) ([]calendar.Permission, error) {
	cal, err := cl.getShared(ctx, calendarID)
	if err != nil {
		return nil, err
	}
	ids := make([]gojmap.ID, 0, len(cal.ShareWith))
	for pid := range cal.ShareWith {
		ids = append(ids, pid)
	}
	slices.Sort(ids) // deterministic order
	out := make([]calendar.Permission, 0, len(ids))
	for _, pid := range ids {
		p := calendar.Permission{ID: string(pid), Role: rightsToRole(cal.ShareWith[pid]), AllowedRoles: allowedRoles(), IsRemovable: true}
		if pr, err := cl.getPrincipal(ctx, pid); err == nil && pr != nil {
			p.Email, p.Name = pr.Email, pr.Name
		}
		out = append(out, p)
	}
	return out, nil
}

// GetCalendarPermission returns the grant whose id is the grantee principal id.
func (cl *Client) GetCalendarPermission(ctx context.Context, calendarID, permissionID string) (calendar.Permission, error) {
	perms, err := cl.ListCalendarPermissions(ctx, calendarID)
	if err != nil {
		return calendar.Permission{}, err
	}
	for _, p := range perms {
		if p.ID == permissionID {
			return p, nil
		}
	}
	return calendar.Permission{}, calendar.ErrPermissionNotFound
}

// CreateCalendarPermission resolves the grantee email to a principal, then grants
// the rights for the requested role on the calendar's shareWith. The returned
// Permission is the stored grant as a later GET would return it.
func (cl *Client) CreateCalendarPermission(ctx context.Context, calendarID string, p calendar.Permission) (calendar.Permission, error) {
	rights, err := rightsForRole(p.Role)
	if err != nil {
		return calendar.Permission{}, err
	}
	pid, err := cl.resolvePrincipal(ctx, p.Email)
	if err != nil {
		return calendar.Permission{}, err
	}
	if err := cl.setShare(ctx, calendarID, pid, rights, false); err != nil {
		return calendar.Permission{}, err
	}
	return cl.GetCalendarPermission(ctx, calendarID, string(pid))
}

// UpdateCalendarPermission changes the role of an existing grant (id = principal
// id); only p.Role is used. The returned Permission is the stored grant as a later
// GET would return it.
func (cl *Client) UpdateCalendarPermission(ctx context.Context, calendarID, permissionID string, p calendar.Permission) (calendar.Permission, error) {
	rights, err := rightsForRole(p.Role)
	if err != nil {
		return calendar.Permission{}, err
	}
	if err := cl.setShare(ctx, calendarID, gojmap.ID(permissionID), rights, true); err != nil {
		return calendar.Permission{}, err
	}
	return cl.GetCalendarPermission(ctx, calendarID, permissionID)
}

// DeleteCalendarPermission revokes a grant by removing the principal from shareWith.
func (cl *Client) DeleteCalendarPermission(ctx context.Context, calendarID, permissionID string) error {
	return cl.setShare(ctx, calendarID, gojmap.ID(permissionID), nil, true)
}

// getShared fetches a calendar with its shareWith map.
func (cl *Client) getShared(ctx context.Context, calendarID string) (*jmapCalendar, error) {
	args, err := cl.do(ctx, &calendarGet{
		Account:    cl.accountID,
		IDs:        []gojmap.ID{gojmap.ID(calendarID)},
		Properties: []string{"id", "name", "shareWith"},
	})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*calendarGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for Calendar/get", args)
	}
	if len(resp.List) == 0 {
		return nil, calendar.ErrCalendarNotFound
	}
	return resp.List[0], nil
}

// getPrincipal resolves one principal's id/email/name.
func (cl *Client) getPrincipal(ctx context.Context, id gojmap.ID) (*jmapPrincipal, error) {
	args, err := cl.do(ctx, &principalGet{Account: cl.principalAccountID(), IDs: []gojmap.ID{id}})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*principalGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for Principal/get", args)
	}
	if len(resp.List) == 0 {
		return nil, fmt.Errorf("jmap: principal %q not found", id)
	}
	return resp.List[0], nil
}

// resolvePrincipal finds the principal id for an email address.
func (cl *Client) resolvePrincipal(ctx context.Context, email string) (gojmap.ID, error) {
	if email == "" {
		return "", fmt.Errorf("jmap: empty grantee email")
	}
	args, err := cl.do(ctx, &principalQuery{Account: cl.principalAccountID(), Filter: &principalFilter{Email: email}})
	if err != nil {
		return "", err
	}
	resp, ok := args.(*principalQueryResponse)
	if !ok {
		return "", fmt.Errorf("jmap: unexpected response %T for Principal/query", args)
	}
	if len(resp.IDs) == 0 {
		return "", fmt.Errorf("jmap: no principal found for %q", email)
	}
	return resp.IDs[0], nil
}

// jsonPointerEscape escapes a path segment per RFC 6901 for a JMAP PatchObject key.
func jsonPointerEscape(seg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(seg, "~", "~0"), "/", "~1")
}

// setShare sets (or, when rights is nil, removes) the grant for one principal with
// a per-principal PatchObject on shareWith ("shareWith/<pid>"), so concurrent
// writers touching other grantees cannot clobber or resurrect each other's
// entries. The granted rights are normalised to satisfy the mayWriteAll
// implication Stalwart 1.x enforces. When requireExisting is set (update/revoke),
// a missing principal yields ErrPermissionNotFound.
func (cl *Client) setShare(ctx context.Context, calendarID string, pid gojmap.ID, rights *calendarRights, requireExisting bool) error {
	if requireExisting {
		cal, err := cl.getShared(ctx, calendarID)
		if err != nil {
			return err
		}
		if _, ok := cal.ShareWith[pid]; !ok {
			return calendar.ErrPermissionNotFound
		}
	}
	var value any // nil marshals to JSON null: remove the grant
	if rights != nil {
		r := *rights
		normalizeRights(&r)
		value = r
	}
	args, err := cl.do(ctx, &calendarSet{
		Account: cl.accountID,
		Update: map[gojmap.ID]map[string]any{
			gojmap.ID(calendarID): {"shareWith/" + jsonPointerEscape(string(pid)): value},
		},
	})
	if err != nil {
		return err
	}
	resp, ok := args.(*calendarSetResponse)
	if !ok {
		return fmt.Errorf("jmap: unexpected response %T for Calendar/set", args)
	}
	if se := resp.NotUpdated[gojmap.ID(calendarID)]; se != nil {
		switch se.Type {
		case "notFound":
			return calendar.ErrCalendarNotFound
		case "invalidProperties", "forbidden":
			// e.g. sharing a calendar with its own owner. The server's description
			// is intentionally dropped (it is backend text, not for the client).
			return calendar.ErrInvalidGrantee
		}
		return fmt.Errorf("jmap: calendar shareWith not updated: %s", se.Type)
	}
	return nil
}

// supportedRoles are the Graph roles the JMAP tier can enforce and read back
// exactly. JMAP cannot express delegate semantics (meeting-request routing),
// private-event hiding, or limitedRead (mayReadItems is all-or-nothing), so those
// roles, and custom, are refused with calendar.ErrRoleNotAllowed rather than
// silently downgraded.
var supportedRoles = []calendar.PermissionRole{
	calendar.RoleNone, calendar.RoleFreeBusyRead, calendar.RoleRead, calendar.RoleWrite,
}

// allowedRoles returns a fresh copy of supportedRoles for Permission.AllowedRoles.
func allowedRoles() []calendar.PermissionRole {
	return slices.Clone(supportedRoles)
}

// rightsForRole validates role and returns its JMAP rights, or ErrRoleNotAllowed.
func rightsForRole(role calendar.PermissionRole) (*calendarRights, error) {
	if !slices.Contains(supportedRoles, role) {
		return nil, fmt.Errorf("%w: %q (this calendar backend supports %v)", calendar.ErrRoleNotAllowed, role, supportedRoles)
	}
	return roleToRights(role), nil
}

// normalizeRights fills in the flags the JMAP Calendars spec implies: mayWriteAll
// implies mayWriteOwn, mayUpdatePrivate and mayRSVP.
func normalizeRights(r *calendarRights) {
	if r != nil && r.MayWriteAll {
		r.MayWriteOwn, r.MayUpdatePrivate, r.MayRSVP = true, true, true
	}
}

// roleToRights maps a supported Graph role onto JMAP per-principal rights. Write
// deliberately omits mayDelete (which in JMAP Calendars deletes the calendar
// itself, not events) and mayShare. Unsupported roles yield no rights; callers
// validate with rightsForRole first.
func roleToRights(role calendar.PermissionRole) *calendarRights {
	r := &calendarRights{}
	switch role {
	case calendar.RoleFreeBusyRead:
		r.MayReadFreeBusy = true
	case calendar.RoleRead:
		r.MayReadFreeBusy, r.MayReadItems = true, true
	case calendar.RoleWrite:
		r.MayReadFreeBusy, r.MayReadItems, r.MayWriteAll, r.MayWriteOwn, r.MayUpdatePrivate, r.MayRSVP = true, true, true, true, true, true
	}
	return r
}

// rightsToRole infers the role from a JMAP rights object, including grants made
// outside the gateway: mayWriteAll is write, else mayReadItems is read, else
// mayReadFreeBusy is freeBusyRead, else none.
func rightsToRole(r *calendarRights) calendar.PermissionRole {
	switch {
	case r == nil:
		return calendar.RoleNone
	case r.MayWriteAll:
		return calendar.RoleWrite
	case r.MayReadItems:
		return calendar.RoleRead
	case r.MayReadFreeBusy:
		return calendar.RoleFreeBusyRead
	default:
		return calendar.RoleNone
	}
}
