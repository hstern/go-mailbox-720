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
	Attendees  []Address
	Body       Body
	Status     string // mapped from the iCalendar STATUS (e.g. "confirmed")
	CreatedAt  time.Time
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
