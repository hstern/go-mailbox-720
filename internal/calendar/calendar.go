// Package calendar defines the calendar backing-store port: a backend-neutral,
// Graph/JMAP-shaped view of calendars and events that the server maps Microsoft
// Graph requests onto. The CalDAV adapter (internal/calendar/caldav) is the
// first implementation; a JMAP-calendars adapter can drop in behind the same
// interface later.
//
// Like the mail port (internal/mail), this port holds no calendar data of its
// own — each method round-trips to the operator's existing CalDAV server.
// Calendar and event IDs are opaque and stable, derived from backend
// identifiers (CalDAV hrefs) so a Graph client can round-trip them.
package calendar

import (
	"context"
	"errors"
	"time"
)

// Address is a parsed calendar-user address (display name + email). It mirrors
// mail.Address; CalDAV carries these as CAL-ADDRESS values (a "mailto:" URI plus
// an optional CN parameter for the display name).
type Address struct {
	Name  string
	Email string
}

// Attendee is an event participant: a calendar-user address plus their
// participation status. Status is backend-neutral, mirroring Graph's
// responseStatus.response (which the CalDAV adapter maps to/from the iCalendar
// PARTSTAT): one of "" (unset), "accepted", "declined", "tentativelyAccepted",
// or "notResponded".
type Attendee struct {
	Name   string
	Email  string
	Status string
	// ScheduleStatus is the RFC 6638 SCHEDULE-STATUS for this attendee: an iTIP
	// REQUEST-STATUS code (e.g. "1.1" sent, "5.1" undeliverable) recording the
	// delivery outcome of the last scheduling message the server sent them. Empty
	// when no scheduling message has been sent (or a native server tracks it
	// out-of-band). The CalDAV adapter carries it as the ATTENDEE SCHEDULE-STATUS
	// parameter; Graph has no equivalent field, so it does not surface in /me/events.
	ScheduleStatus string
}

// Body is an event description in a single representation. Calendars almost
// always carry plain text (the iCalendar DESCRIPTION property), so ContentType
// is "text" in the common case.
type Body struct {
	ContentType string // "text" or "html"
	Content     string
}

// Calendar is a calendar collection in Graph/JMAP object shape.
type Calendar struct {
	ID          string
	Name        string
	Description string
}

// Event is a calendar event in Graph/JMAP object shape. List operations populate
// the cheap envelope-level fields; richer fields (Body, Attendees) are also
// available because CalDAV returns whole VEVENTs rather than a cheap envelope.
type Event struct {
	ID         string
	CalendarID string
	UID        string // the iCalendar UID, stable across the event's lifetime
	Subject    string
	Start      time.Time
	End        time.Time
	IsAllDay   bool
	Location   string
	Organizer  Address
	Attendees  []Attendee
	Body       Body
	Status     string // mapped from the iCalendar STATUS (e.g. "confirmed")
	// Sequence is the iCalendar SEQUENCE (RFC 5545 §3.8.7.4): the event's revision
	// number, bumped by the organizer on each significant change so iTIP recipients
	// can tell a re-sent REQUEST/CANCEL supersedes an earlier one. 0 for a new event.
	Sequence  int
	CreatedAt time.Time

	// Recurrence describes the repeat rule of a recurring series, carried on the
	// series master VEVENT (the iCalendar RRULE plus its EXDATE exceptions). Nil on
	// a non-recurring event and on an individual occurrence/override; the master
	// owns the pattern. It maps to Graph's event.recurrence (patternedRecurrence).
	Recurrence *RecurrencePattern

	// RecurrenceID marks an event that addresses a single instance of a recurring
	// series rather than the whole series: it is the original start instant of the
	// occurrence the event stands for (the iCalendar RECURRENCE-ID). Zero on a
	// non-recurring event and on the series master. An occurrence surfaced by
	// expansion and an override (exception) VEVENT both carry it; IsOverride
	// distinguishes them.
	RecurrenceID time.Time

	// IsOverride reports whether this instance is an exception VEVENT explicitly
	// stored by the organizer (an override of the pattern, e.g. a moved or
	// retitled occurrence) rather than a plain occurrence synthesized from the
	// master's RRULE. It is meaningful only when RecurrenceID is non-zero. Maps to
	// Graph's event.type ("exception" vs "occurrence").
	IsOverride bool

	// SeriesMasterID is the opaque ID of the series master event for an instance
	// (occurrence or override). Empty on the master itself and on non-recurring
	// events. It backs Graph's event.seriesMasterId so a client can navigate from
	// an instance back to the series.
	SeriesMasterID string
}

// RecurrencePattern is the backend-neutral recurrence rule of a series: the
// iCalendar RRULE together with its EXDATE exception instants. It is modelled
// around the iCalendar RRULE rather than Graph's split pattern/range so the
// CalDAV adapter can round-trip it losslessly; the Graph layer derives the
// patternedRecurrence shape (recurrencePattern + recurrenceRange) from it.
type RecurrencePattern struct {
	// RRULE is the iCalendar recurrence rule value (RFC 5545 §3.3.10), e.g.
	// "FREQ=WEEKLY;BYDAY=MO,WE;COUNT=10" — the RRULE property value with the
	// "RRULE:" name stripped. It is the canonical form; the adapter writes it back
	// verbatim and the Graph layer parses it for patternedRecurrence.
	RRULE string

	// ExceptionDates are the EXDATE instants (RFC 5545 §3.8.5.1): occurrences the
	// rule would generate but that have been removed from the series. Carried in
	// UTC.
	ExceptionDates []time.Time
}

// Range bounds an event listing by start/end instant. A zero Start or End means
// unbounded on that side; the zero Range lists everything the backend returns.
type Range struct {
	Start time.Time
	End   time.Time
}

// Backend is the calendar backing-store port. Implementations adapt a concrete
// server (CalDAV first) to this neutral shape. A Backend is bound to a single
// authenticated principal.
//
// First cut: the read paths only. Deferred to their own issues (mirroring the
// mail port): change subscriptions / push, delta sync tokens, $filter execution,
// and event creation / modification.
type Backend interface {
	// ListCalendars returns the principal's calendar collections.
	ListCalendars(ctx context.Context) ([]Calendar, error)
	// ListEvents returns events in a calendar, optionally bounded by a time range.
	ListEvents(ctx context.Context, calendarID string, r Range) ([]Event, error)
	// GetEvent returns a single event by opaque ID.
	GetEvent(ctx context.Context, id string) (Event, error)
	// Close releases the backend connection.
	Close() error
}

// Writer is the optional event write capability: create, update, and delete.
// It is kept separate from Backend so that a read-only adapter (or the server's
// read-path fakes) need not implement writes, and so that adding writes does not
// disturb Backend's existing implementers. An adapter that supports writes
// implements Writer in addition to Backend; consumers type-assert for it:
//
//	if w, ok := backend.(calendar.Writer); ok {
//		created, err := w.CreateEvent(ctx, calendarID, e)
//	}
//
// A Writer is bound to the same authenticated principal as its Backend.
type Writer interface {
	// CreateEvent creates a new event in the named calendar and returns it
	// stamped with its assigned opaque ID (and, when the backend generates one,
	// its UID). The input event's ID is ignored.
	CreateEvent(ctx context.Context, calendarID string, e Event) (Event, error)
	// UpdateEvent replaces the event identified by e.ID with e and returns the
	// stored event. The opaque e.ID locates the backing resource; CalendarID is
	// derived from it.
	UpdateEvent(ctx context.Context, e Event) (Event, error)
	// DeleteEvent removes the event with the given opaque ID. Deleting an event
	// that does not exist returns a not-found error (mirroring Graph's DELETE
	// semantics); a caller wanting idempotent cleanup can ignore it.
	DeleteEvent(ctx context.Context, id string) error
}

// DeltaReader is the optional incremental-sync capability: report the events
// that have changed in a calendar since a prior point, identified by an opaque
// token. It is kept separate from Backend (like Writer) so that an adapter
// without delta support, and the server's read-path fakes, need not implement
// it, and so adding it does not disturb Backend's existing implementers. An
// adapter that supports delta implements DeltaReader in addition to Backend;
// consumers type-assert for it:
//
//	if d, ok := backend.(calendar.DeltaReader); ok {
//		events, next, err := d.Delta(ctx, calendarID, token)
//	}
//
// This is the backing for Microsoft Graph's GET /me/events/delta. A
// DeltaReader is bound to the same authenticated principal as its Backend.
type DeltaReader interface {
	// Delta returns the events in the calendar changed since the opaque token
	// (an RFC 6578 sync-token). An empty token means initial sync: all current
	// events + a fresh token. The returned next token is fed back next call.
	//
	// changed holds created/updated events; removed holds the opaque IDs of events
	// the sync-collection reported as deleted (so the handler can emit Graph
	// @removed tombstones). On an initial sync removed is empty.
	Delta(ctx context.Context, calendarID string, token string) (changed []Event, removed []string, next string, err error)
}

// SchedulingDetector is the optional capability to report whether the backing
// server performs iTIP scheduling itself (CalDAV RFC 6638 calendar
// auto-scheduling). It is kept separate from Backend (like Writer); an adapter
// that can detect it implements SchedulingDetector in addition to Backend, and
// the scheduling trigger type-asserts for it:
//
//	if d, ok := backend.(calendar.SchedulingDetector); ok { ... }
//
// When the server schedules natively, the client-side email scheduling bridge
// (inbound REQUEST mail → tentative event) should not run, since the server
// already handles inbound iTIP — running both would duplicate events.
type SchedulingDetector interface {
	// SupportsServerScheduling reports whether the server implements RFC 6638
	// calendar auto-scheduling.
	SupportsServerScheduling(ctx context.Context) (bool, error)
}

// Finder is the optional capability to locate an event by its iCalendar UID rather
// than the opaque store ID. The inbound scheduling trigger uses it to correlate a
// re-sent REQUEST with the tentative event an earlier REQUEST created, so a new
// revision updates that event in place instead of creating a duplicate. An adapter
// that can index by UID implements Finder in addition to Backend; callers fall back
// to creating when it is absent.
type Finder interface {
	// FindEventByUID returns the event in the named calendar whose iCalendar UID
	// matches uid, and whether one was found. A nil error with found=false means no
	// such event; an error means the lookup itself failed.
	FindEventByUID(ctx context.Context, calendarID, uid string) (Event, bool, error)
}

// InstanceReader is the optional capability to surface the individual occurrences
// of a recurring series as addressable events: expand the master's RRULE over a
// window and merge in the stored override (RECURRENCE-ID) VEVENTs. It backs
// Microsoft Graph's GET /me/events/{id}/instances and the occurrence expansion of
// GET /me/calendarView. It is kept separate from Backend (like Writer and Finder)
// so an adapter without recurrence expansion, and the server's read-path fakes,
// need not implement it; consumers type-assert for it:
//
//	if ir, ok := backend.(calendar.InstanceReader); ok {
//		insts, err := ir.ListInstances(ctx, eventID, r)
//	}
//
// An InstanceReader is bound to the same authenticated principal as its Backend.
type InstanceReader interface {
	// ListInstances expands the recurring series identified by the opaque event ID
	// (the series master) into its occurrences within the time range r, which must
	// be bounded on both sides — an unbounded RRULE (no COUNT/UNTIL) has infinitely
	// many occurrences, so the caller supplies the window. Each returned Event
	// carries a RecurrenceID (the occurrence's original start) and SeriesMasterID
	// (the master's opaque ID). Occurrences with a stored override take the
	// override's fields (and IsOverride=true); EXDATE-cancelled occurrences are
	// omitted. A non-recurring event yields the single event itself when it falls
	// in the window.
	ListInstances(ctx context.Context, eventID string, r Range) ([]Event, error)
}

// InstanceWriter is the optional capability to record a per-occurrence override of
// a recurring series: store an exception (RECURRENCE-ID) event that replaces a
// single occurrence's fields without disturbing the master rule or the other
// occurrences. It is kept separate from Writer (like InstanceReader from Backend)
// so an adapter without per-instance writes need not implement it; consumers
// type-assert for it:
//
//	if iw, ok := backend.(calendar.InstanceWriter); ok {
//		stored, err := iw.WriteInstanceOverride(ctx, masterID, override)
//	}
//
// An InstanceWriter is bound to the same authenticated principal as its Backend.
type InstanceWriter interface {
	// WriteInstanceOverride adds or replaces the override for one occurrence of the
	// series identified by masterID (its opaque series-master ID). override carries
	// the occurrence's RecurrenceID (which occurrence) plus the replacement fields;
	// RecurrenceID must be non-zero. It returns the stored override stamped with its
	// instance ID, CalendarID, and SeriesMasterID.
	WriteInstanceOverride(ctx context.Context, masterID string, override Event) (Event, error)
}

// PermissionRole is a backend-neutral calendar-sharing role, mirroring Microsoft
// Graph's calendarRoleType: the level of access a grantee has to a shared calendar.
type PermissionRole string

// The calendarRoleType vocabulary (Graph). Backends map these onto their own rights
// model (e.g. JMAP Sharing shareWith rights).
const (
	RoleNone            PermissionRole = "none"
	RoleFreeBusyRead    PermissionRole = "freeBusyRead"
	RoleLimitedRead     PermissionRole = "limitedRead"
	RoleRead            PermissionRole = "read"
	RoleWrite           PermissionRole = "write"
	RoleDelegate        PermissionRole = "delegateWithoutPrivateEventAccess"
	RoleDelegatePrivate PermissionRole = "delegateWithPrivateEventAccess"
	RoleCustom          PermissionRole = "custom"
)

// Permission is a backend-neutral calendar-sharing grant: an identity (the grantee's
// email and display name) and the role it holds on the calendar. It is the neutral
// shape the server maps Microsoft Graph's calendarPermission onto, and that a
// PermissionReader/PermissionWriter adapter translates to the backend sharing
// mechanism (JMAP Sharing shareWith on the JMAP tier; MB720-24).
type Permission struct {
	// ID is an opaque, stable identifier for the grant within its calendar. Empty on
	// a grant being created; the PermissionWriter assigns it.
	ID string
	// Email is the grantee's address; Name its display name (best-effort).
	Email string
	Name  string
	// Role is the access level granted. AllowedRoles, when known, lists the roles the
	// grantee could be assigned (Graph surfaces it read-only).
	Role         PermissionRole
	AllowedRoles []PermissionRole
	// IsInsideOrganization reports whether the grantee is in the owner's organization;
	// IsRemovable whether the grant can be removed (false for the owner's own entry).
	IsInsideOrganization bool
	IsRemovable          bool
}

// PermissionReader is the optional capability to read a calendar's sharing grants —
// who it is shared with and their roles. It is kept separate from Backend so an
// adapter without sharing support need not implement it; consumers type-assert:
//
//	if pr, ok := backend.(calendar.PermissionReader); ok {
//		perms, err := pr.ListCalendarPermissions(ctx, calendarID)
//	}
//
// It backs Graph's GET .../calendars/{id}/calendarPermissions and .../{id}. A
// PermissionReader is bound to the same authenticated principal as its Backend.
type PermissionReader interface {
	// ListCalendarPermissions returns the calendar's sharing grants. The MVP returns
	// the explicit grantees only; it does not synthesize the owner's own entry that
	// Graph normally includes (so IsRemovable is true on every returned grant).
	ListCalendarPermissions(ctx context.Context, calendarID string) ([]Permission, error)
	// GetCalendarPermission returns the grant with the given opaque id, or
	// ErrPermissionNotFound.
	GetCalendarPermission(ctx context.Context, calendarID, permissionID string) (Permission, error)
}

// PermissionWriter is the optional capability to grant, change, and revoke calendar
// sharing, backing Graph's POST/PATCH/DELETE .../calendarPermissions. Like
// PermissionReader it is type-asserted and bound to the same authenticated principal.
type PermissionWriter interface {
	// CreateCalendarPermission grants p (whose ID is empty) on the calendar and
	// returns it with the backend-assigned ID.
	CreateCalendarPermission(ctx context.Context, calendarID string, p Permission) (Permission, error)
	// UpdateCalendarPermission changes the grant with the given id, returning the
	// updated grant, or ErrPermissionNotFound.
	UpdateCalendarPermission(ctx context.Context, calendarID, permissionID string, p Permission) (Permission, error)
	// DeleteCalendarPermission revokes the grant with the given id, or returns
	// ErrPermissionNotFound.
	DeleteCalendarPermission(ctx context.Context, calendarID, permissionID string) error
}

// ErrPermissionNotFound is returned by PermissionReader/PermissionWriter when no
// sharing grant has the requested id.
var ErrPermissionNotFound = errors.New("calendar: permission not found")
