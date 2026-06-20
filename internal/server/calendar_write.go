package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/itip"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
	"github.com/hstern/go-mailbox-720/internal/tz"
)

// MeCreateEvents implements POST /me/events. It maps the inbound Graph event
// body onto the neutral calendar.Event, creates it in the principal's primary
// calendar, and returns the stored event (201 Created). The backend is obtained
// via calendarBackend (nil-provider -> 501) and type-asserted to calendar.Writer;
// a read-only backend yields 501.
func (h Handler) MeCreateEvents(ctx context.Context, req *api.MicrosoftGraphEvent) (api.MeCreateEventsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(calendar.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	event, err := graphToEvent(req)
	if err != nil {
		return badRequest(err.Error()), nil
	}

	calID, ok, err := defaultCalendarID(ctx, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No calendar to create the event in; nothing to write against.
		return nil, ht.ErrNotImplemented
	}

	created, err := w.CreateEvent(ctx, calID, event)
	if err != nil {
		return nil, fmt.Errorf("create event: %w", err)
	}
	// Organizer side of the iTIP engine: email the attendees a METHOD:REQUEST and
	// record the delivery outcome. Best-effort — the create has already succeeded.
	created = h.inviteAttendees(ctx, b, w, created)
	return &api.MicrosoftGraphEventStatusCode{
		StatusCode: http.StatusCreated,
		Response:   toGraphEvent(created),
	}, nil
}

// inviteAttendees is the organizer half of the iTIP engine: when an event is
// created with attendees on the dumb-backend tier, it emails them a METHOD:REQUEST
// iMIP invitation and records the per-attendee delivery outcome as an RFC 6638
// SCHEDULE-STATUS, returning the event with those statuses (and the creator stamped
// as organizer) persisted.
//
// It no-ops unless there is something to send AND we are the scheduler: no
// scheduling provider, no known mailbox address, no attendees besides the
// organizer, or a backend that schedules natively (RFC 6638) — in which case the
// server sends the invitations itself and double-driving must be avoided.
//
// Per Graph/Exchange semantics the create itself always succeeds (201); a send
// failure is never an HTTP error. It is surfaced out of band instead — logged, and
// recorded as SCHEDULE-STATUS=5.1 on the attendees (the protocol's delivery-status
// channel, read back via CalDAV) — never silently swallowed.
func (h Handler) inviteAttendees(ctx context.Context, b calendar.Backend, w calendar.Writer, event calendar.Event) calendar.Event {
	if h.scheduling == nil {
		return event
	}
	organizer := h.scheduling.MailboxAddress()
	if organizer == "" {
		return event
	}
	isRecipient := func(a calendar.Attendee) bool {
		return a.Email != "" && !strings.EqualFold(a.Email, organizer)
	}
	hasRecipient := false
	for _, a := range event.Attendees {
		if isRecipient(a) {
			hasRecipient = true
			break
		}
	}
	if !hasRecipient {
		return event
	}
	// Capability switch: an RFC 6638 server schedules on our behalf.
	if native, _ := serverSchedulesEvents(ctx, b); native {
		return event
	}

	status := h.sendRequest(ctx, organizer, event)

	// Record the outcome where RFC 6638 puts it — a per-attendee SCHEDULE-STATUS —
	// and stamp the creator as organizer (you organize events you invite others to).
	// Persisted via UpdateEvent so a later CalDAV read reflects it.
	if !strings.EqualFold(event.Organizer.Email, organizer) {
		event.Organizer = calendar.Address{Email: organizer}
	}
	for i := range event.Attendees {
		if isRecipient(event.Attendees[i]) {
			event.Attendees[i].ScheduleStatus = string(status)
		}
	}
	updated, err := w.UpdateEvent(ctx, event)
	if err != nil {
		log.Printf("calendar: persist SCHEDULE-STATUS for event %s: %v", event.ID, err)
		return event
	}
	return updated
}

// sendRequest submits the METHOD:REQUEST iMIP invitation to the event's attendees
// (other than the organizer) and returns the resulting SCHEDULE-STATUS. A failure
// is logged and mapped to SchedStatusNoDelivery, never returned as an error: per
// Graph/Exchange the create succeeds regardless of invitation delivery.
func (h Handler) sendRequest(ctx context.Context, organizer string, event calendar.Event) scheduling.ScheduleStatus {
	inv := itip.InviteFromEvent(event)
	// Force the organizer to the configured mailbox, but keep the display name the
	// client supplied when they already named themselves as organizer (CN is lost
	// only when we have to synthesize the organizer, where no name is available).
	if !strings.EqualFold(inv.Organizer.Email, organizer) {
		inv.Organizer = scheduling.Address{Email: organizer}
	}
	recipients := make([]scheduling.Attendee, 0, len(inv.Attendees))
	for _, a := range inv.Attendees {
		if a.Email != "" && !strings.EqualFold(a.Email, organizer) {
			recipients = append(recipients, a)
		}
	}
	inv.Attendees = recipients

	sender, err := h.scheduling.Sender(ctx)
	if err != nil {
		log.Printf("calendar: iMIP REQUEST for event %s: smtp sender: %v", event.ID, err)
		return scheduling.SchedStatusNoDelivery
	}
	defer func() { _ = sender.Close() }()

	if err := itip.Invite(ctx, sender, inv.Organizer, inv, time.Now()); err != nil {
		log.Printf("calendar: iMIP REQUEST send failed for event %s: %v", event.ID, err)
		return scheduling.SchedStatusNoDelivery
	}
	return scheduling.SchedStatusSent
}

// MeUpdateEvents implements PATCH /me/events/{event-id}. PATCH is a partial
// update: the current event is read via GetEvent and only the fields present in
// the inbound Graph body overlay it (absent fields are left unchanged), then the
// merged event — preserving its ID/UID — is written via Writer.UpdateEvent and
// returned (200 OK). The backend is obtained via calendarBackend (nil-provider
// -> 501) and type-asserted to calendar.Writer; a read-only backend yields 501.
// A non-UTC time zone on a patched Start/End is rejected with 400, as in create.
func (h Handler) MeUpdateEvents(ctx context.Context, req *api.MicrosoftGraphEvent, params api.MeUpdateEventsParams) (api.MeUpdateEventsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(calendar.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	current, err := b.GetEvent(ctx, params.EventID)
	if err != nil {
		return nil, fmt.Errorf("get event: %w", err)
	}

	merged, err := mergeEventPatch(current, req)
	if err != nil {
		return badRequest(err.Error()), nil
	}

	updated, err := w.UpdateEvent(ctx, merged)
	if err != nil {
		return nil, fmt.Errorf("update event: %w", err)
	}
	return &api.MicrosoftGraphEventStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphEvent(updated),
	}, nil
}

// MeDeleteEvents implements DELETE /me/events/{event-id}. It type-asserts the
// backend to calendar.Writer (read-only backend -> 501) and deletes the event,
// returning 204 No Content.
func (h Handler) MeDeleteEvents(ctx context.Context, params api.MeDeleteEventsParams) (api.MeDeleteEventsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(calendar.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	if err := w.DeleteEvent(ctx, params.EventID); err != nil {
		return nil, fmt.Errorf("delete event: %w", err)
	}
	return &api.MeDeleteEventsNoContent{}, nil
}

// graphToEvent maps the inbound Graph event onto the neutral calendar.Event — the
// inverse of toGraphEvent. Read-only and server-assigned fields (ID, ICalUId) are
// ignored: the backend stamps the created event with its own opaque ID/UID.
func graphToEvent(ge *api.MicrosoftGraphEvent) (calendar.Event, error) {
	e := calendar.Event{
		Subject:   ge.Subject.Or(""),
		IsAllDay:  ge.IsAllDay.Or(false),
		Attendees: graphToAttendees(ge.Attendees),
	}
	if v, ok := ge.Start.Get(); ok {
		t, err := graphToTime(v)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("start: %w", err)
		}
		e.Start = t
	}
	if v, ok := ge.End.Get(); ok {
		t, err := graphToTime(v)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("end: %w", err)
		}
		e.End = t
	}
	if v, ok := ge.Location.Get(); ok {
		e.Location = v.DisplayName.Or("")
	}
	if v, ok := ge.Organizer.Get(); ok {
		e.Organizer = graphRecipientToAddress(v)
	}
	if v, ok := ge.Body.Get(); ok {
		e.Body = calendar.Body{
			Content:     v.Content.Or(""),
			ContentType: neutralBodyType(v.ContentType),
		}
	}
	return e, nil
}

// mergeEventPatch overlays the fields present in the inbound Graph PATCH body
// onto the current event, leaving absent fields unchanged — the read-modify-write
// half of PATCH semantics. Presence is detected per field: scalar Opt/OptNil
// fields via .Get() (a set field overlays, even when its value is empty), and the
// Attendees collection via a non-empty slice. The event's identity (ID, UID, and
// the rest of the current record) is preserved so UpdateEvent rewrites in place.
// A patched Start/End with a non-UTC time zone is rejected just like create.
func mergeEventPatch(current calendar.Event, ge *api.MicrosoftGraphEvent) (calendar.Event, error) {
	merged := current
	if v, ok := ge.Subject.Get(); ok {
		merged.Subject = v
	}
	if v, ok := ge.IsAllDay.Get(); ok {
		merged.IsAllDay = v
	}
	if v, ok := ge.Start.Get(); ok {
		t, err := graphToTime(v)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("start: %w", err)
		}
		merged.Start = t
	}
	if v, ok := ge.End.Get(); ok {
		t, err := graphToTime(v)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("end: %w", err)
		}
		merged.End = t
	}
	if v, ok := ge.Location.Get(); ok {
		merged.Location = v.DisplayName.Or("")
	}
	if v, ok := ge.Organizer.Get(); ok {
		merged.Organizer = graphRecipientToAddress(v)
	}
	if v, ok := ge.Body.Get(); ok {
		merged.Body = calendar.Body{
			Content:     v.Content.Or(""),
			ContentType: neutralBodyType(v.ContentType),
		}
	}
	if len(ge.Attendees) > 0 {
		merged.Attendees = graphToAttendees(ge.Attendees)
	}
	return merged, nil
}

// graphToTime parses a Graph dateTimeTimeZone back into an instant — the inverse
// of graphDateTime. An RFC3339 dateTime carries its own offset and is honored as
// given. Otherwise the dateTime is a naive wall-clock interpreted in the event's
// timeZone: Graph sends Windows zone names like "Pacific Standard Time" (resolved
// via internal/tz), IANA names, or "UTC"/absent (treated as UTC). An unknown zone
// is a 400-worthy error rather than a silent mis-store; an absent or unparseable
// dateTime yields the zero time, which the backend treats as unset.
func graphToTime(dt api.MicrosoftGraphDateTimeTimeZone) (time.Time, error) {
	s, ok := dt.DateTime.Get()
	if !ok || s == "" {
		return time.Time{}, nil
	}
	// An RFC3339 instant fixes its own offset; honor it regardless of timeZone.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	// Naive wall-clock: interpret it in the declared zone, then normalize to UTC.
	loc := time.UTC
	if name, ok := dt.TimeZone.Get(); ok && name != "" && !strings.EqualFold(name, "UTC") {
		l, err := tz.Lookup(name)
		if err != nil {
			return time.Time{}, fmt.Errorf("time zone: %w", err)
		}
		loc = l
	}
	for _, layout := range []string{"2006-01-02T15:04:05.0000000", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, nil
}

// graphRecipientToAddress maps a Graph recipient onto a calendar.Address.
func graphRecipientToAddress(r api.MicrosoftGraphRecipient) calendar.Address {
	ea, ok := r.EmailAddress.Get()
	if !ok {
		return calendar.Address{}
	}
	return calendar.Address{
		Name:  ea.Name.Or(""),
		Email: ea.Address.Or(""),
	}
}

// graphToAddresses maps Graph attendees onto neutral calendar.Address values.
func graphToAttendees(as []api.MicrosoftGraphAttendee) []calendar.Attendee {
	if len(as) == 0 {
		return nil
	}
	out := make([]calendar.Attendee, 0, len(as))
	for _, a := range as {
		ea, ok := a.EmailAddress.Get()
		if !ok {
			continue
		}
		att := calendar.Attendee{
			Name:  ea.Name.Or(""),
			Email: ea.Address.Or(""),
		}
		// Graph's responseStatus.response uses the same tokens as the neutral status.
		if rs, ok := a.Status.Get(); ok {
			if rt, ok := rs.Response.Get(); ok {
				att.Status = string(rt)
			}
		}
		out = append(out, att)
	}
	return out
}

// neutralBodyType maps a Graph bodyType back onto the neutral "text"/"html"
// string — the inverse of graphBodyType.
func neutralBodyType(bt api.OptMicrosoftGraphBodyType) string {
	if v, ok := bt.Get(); ok && v == api.MicrosoftGraphBodyTypeHTML {
		return "html"
	}
	return "text"
}
