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
