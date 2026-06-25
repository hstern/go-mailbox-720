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

	"github.com/hstern/go-jscalendar"
)

// Calendar is a calendar collection in Graph/JMAP object shape.
type Calendar struct {
	ID          string
	Name        string
	Description string
}

// Event is a calendar event in the standardized JSCalendar (RFC 8984) object
// shape. It embeds [jscalendar.Event] — the neutral pivot model the whole
// calendar subsystem speaks — and adds the opaque, backend-derived routing IDs
// that are ours, not JSCalendar's: the store/Graph IDs a client round-trips and
// the series linkage. The CalDAV adapter maps iCal↔[jscalendar.Event] via the
// go-jscalendar/ical bridge; the JMAP adapter carries it natively; the server
// maps it to/from the Microsoft Graph event DTO.
//
// The embedded fields carry the event content: UID, Title, Start (+TimeZone,
// Duration), Status, Sequence, Created, Description, Participants, Locations,
// RecurrenceRules, RecurrenceOverrides, RecurrenceID, and the rest of RFC 8984.
// Recurrence and time zones are modelled structurally and losslessly here,
// unlike the bespoke RRULE-string pivot this type replaced (MB720-49/26).
type Event struct {
	// ID is the opaque, stable store/Graph identifier (derived from the backend
	// resource, e.g. a CalDAV href). Distinct from the embedded JSCalendar UID.
	ID string
	// CalendarID is the opaque ID of the calendar collection the event lives in.
	CalendarID string
	// SeriesMasterID is the opaque ID of the series master for an instance
	// (occurrence or override). Empty on the master itself and on non-recurring
	// events. It backs Graph's event.seriesMasterId.
	SeriesMasterID string
	// IsOverride reports whether this instance is an exception explicitly stored
	// by the organizer (a moved or retitled occurrence) rather than a plain
	// occurrence synthesized from the master's rules. Meaningful only when the
	// embedded RecurrenceID is set. Maps to Graph's event.type ("exception" vs
	// "occurrence").
	IsOverride bool
	// ETag is the backend's opaque entity-tag for the event's object resource
	// (the CalDAV getetag), as read on GET/list. It backs Graph's @odata.etag and
	// is the precondition a conditional update (ConditionalWriter) sends back as
	// If-Match. Empty when the backend reports no ETag. It is a property of the
	// stored resource, not of the event's content, so it sits here rather than on
	// the embedded JSCalendar object.
	ETag string

	jscalendar.Event
}

// StartTime resolves the embedded JSCalendar Start + TimeZone to a UTC instant,
// the form the server's range filtering and recurrence expansion compare
// against. A floating Start (no TimeZone) is interpreted as UTC wall-clock. The
// zero time is returned when Start is unset.
func (e Event) StartTime() time.Time {
	return localToUTC(e.Start, e.TimeZone)
}

// EndTime resolves the event's end instant from Start plus the embedded
// JSCalendar Duration (default zero-length). The Duration is applied to the
// wall-clock start in the event's own time zone so that nominal calendar units
// (days, weeks) stay wall-clock-anchored across DST transitions, per iCalendar
// DURATION semantics; the result is returned as a UTC instant. Zero when Start
// is unset.
func (e Event) EndTime() time.Time {
	local := localTime(e.Start, e.TimeZone)
	if local.IsZero() {
		return time.Time{}
	}
	return addDuration(local, e.Duration).UTC()
}

// localTime renders a JSCalendar LocalDateTime as a time.Time in its own time
// zone (UTC fallback for floating or unrecognized zones), without converting to
// UTC — the form to add nominal durations against. Zero when ldt is nil.
func localTime(ldt *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId) time.Time {
	if ldt == nil {
		return time.Time{}
	}
	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(string(tz)); err == nil {
			loc = l
		}
	}
	return time.Date(ldt.Year, time.Month(ldt.Month), ldt.Day,
		ldt.Hour, ldt.Minute, ldt.Second, 0, loc)
}

// localToUTC converts a JSCalendar LocalDateTime in the given time zone to a UTC
// instant. When tz is empty the time is treated as floating (UTC wall-clock).
func localToUTC(ldt *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId) time.Time {
	return localTime(ldt, tz).UTC()
}

// addDuration adds a JSCalendar Duration to a time.Time, matching iCalendar
// DURATION semantics: nominal calendar units (years, months, weeks, days) via
// AddDate — which preserves wall-clock time across DST when t carries a
// DST-observing location — and exact sub-day units via Add. Each uint64 sub-day
// component is clamped to guard against an adversarial wire value overflowing
// time.Duration (int64 ns).
func addDuration(t time.Time, d *jscalendar.Duration) time.Time {
	if d == nil {
		return t
	}
	const maxSubDayUnit = uint64(1<<31 - 1)
	clamp := func(v uint64) time.Duration {
		if v > maxSubDayUnit {
			v = maxSubDayUnit
		}
		return time.Duration(v)
	}
	t = t.AddDate(int(d.Years), int(d.Months), int(d.Days+d.Weeks*7))
	t = t.Add(clamp(d.Hours)*time.Hour +
		clamp(d.Minutes)*time.Minute +
		clamp(d.Seconds)*time.Second +
		time.Duration(d.Nanos))
	return t
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

// ErrPreconditionFailed is returned by a ConditionalWriter when an If-Match
// precondition fails: the stored resource's ETag no longer matches the value the
// caller supplied, meaning the event changed since the caller last read it (a
// lost-update conflict). The HTTP layer maps it to 412 Precondition Failed.
var ErrPreconditionFailed = errors.New("calendar: precondition failed (ETag mismatch)")

// ConditionalWriter is the optional capability to update an event only if its
// stored ETag still matches a caller-supplied value — optimistic concurrency,
// the backing for Microsoft Graph's PATCH /me/events/{id} carrying an If-Match
// header. It is kept separate from Writer (like the other capabilities) so that
// an adapter without conditional support need not implement it; consumers
// type-assert for it:
//
//	if cw, ok := backend.(calendar.ConditionalWriter); ok {
//		updated, err := cw.UpdateEventIfMatch(ctx, e, ifMatch)
//	}
//
// The CalDAV adapter implements it by issuing a conditional PUT (If-Match) so the
// DAV server enforces the precondition atomically, with no read-modify-write
// race. A backend without an ETag concept — the JMAP calendar adapter (which
// surfaces no per-event ETag) and the server's fakes — does not implement it, and
// the server falls back to an unconditional Writer.UpdateEvent, ignoring If-Match.
// A ConditionalWriter is bound to the same authenticated principal as its
// Backend.
type ConditionalWriter interface {
	// UpdateEventIfMatch replaces the event identified by e.ID with e, but only if
	// the event's current ETag equals ifMatch, and returns the stored event. It
	// returns ErrPreconditionFailed when the precondition fails. ifMatch is the
	// opaque ETag the caller last observed (Graph's If-Match); an empty ifMatch is
	// an error — callers with no precondition use Writer.UpdateEvent instead.
	UpdateEventIfMatch(ctx context.Context, e Event, ifMatch string) (Event, error)
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
