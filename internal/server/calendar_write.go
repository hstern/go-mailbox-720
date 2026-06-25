package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-jscalendar"

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
	organizer, send := h.shouldSelfSchedule(ctx, b, event)
	if !send {
		return event
	}

	status := h.sendRequest(ctx, organizer, event)
	// Record the outcome where RFC 6638 puts it — per-attendee SCHEDULE-STATUS, plus
	// the creator stamped as organizer — persisted via UpdateEvent for a later read.
	recordSchedulingOutcome(&event, organizer, status)
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
	inv.Organizer = organizerFor(inv.Organizer, organizer)
	inv.Attendees = schedulingRecipients(inv.Attendees, organizer)

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

// cancelInvitations is the organizer-side withdrawal: when an event with attendees
// is deleted on the dumb-backend tier, it emails them a METHOD:CANCEL iMIP message
// so their calendars drop the meeting. Like inviteAttendees it no-ops unless we are
// the scheduler (same capability switch); unlike it there is nothing to persist —
// the event is already gone — so a send failure is only logged, never swallowed
// into a misleading success or surfaced as an HTTP error (the delete stands).
func (h Handler) cancelInvitations(ctx context.Context, b calendar.Backend, event calendar.Event) {
	organizer, send := h.shouldSelfSchedule(ctx, b, event)
	if !send {
		return
	}
	h.sendCancelTo(ctx, organizer, event, eventRecipients(event, organizer))
}

// sendCancelTo emails a METHOD:CANCEL for event to the given recipients (already
// filtered to exclude the organizer), withdrawing the meeting from their calendars.
// It is the send core shared by a full delete (all attendees) and a PATCH that drops
// some attendees (just those). A send failure is only logged — there is nothing to
// persist, and the calendar change has already happened.
func (h Handler) sendCancelTo(ctx context.Context, organizer string, event calendar.Event, recipients []scheduling.Attendee) {
	if len(recipients) == 0 {
		return
	}
	// itip.Cancel emits METHOD:CANCEL regardless of inv.Method (scheduling.Cancel
	// sets it), so InviteFromEvent's REQUEST method is harmless and left as-is.
	inv := itip.InviteFromEvent(event)
	inv.Organizer = organizerFor(inv.Organizer, organizer)
	inv.Attendees = recipients

	sender, err := h.scheduling.Sender(ctx)
	if err != nil {
		log.Printf("calendar: iMIP CANCEL for event %s: smtp sender: %v", event.ID, err)
		return
	}
	defer func() { _ = sender.Close() }()

	if err := itip.Cancel(ctx, sender, inv.Organizer, inv, time.Now()); err != nil {
		log.Printf("calendar: iMIP CANCEL send failed for event %s: %v", event.ID, err)
	}
}

// eventRecipients is the event's scheduling recipients: its attendees other than
// the organizer, as scheduling.Attendee values.
func eventRecipients(event calendar.Event, organizer string) []scheduling.Attendee {
	return schedulingRecipients(itip.InviteFromEvent(event).Attendees, organizer)
}

// removedRecipients returns the recipients on current that are absent from merged —
// the attendees a PATCH dropped, who are owed a CANCEL. Matching is by email,
// case-insensitively.
func removedRecipients(current, merged calendar.Event, organizer string) []scheduling.Attendee {
	keep := make(map[string]bool)
	for _, a := range eventRecipients(merged, organizer) {
		keep[strings.ToLower(a.Email)] = true
	}
	var out []scheduling.Attendee
	for _, a := range eventRecipients(current, organizer) {
		if !keep[strings.ToLower(a.Email)] {
			out = append(out, a)
		}
	}
	return out
}

// rosterAdded reports whether merged has a recipient that current lacks (a PATCH
// added an attendee), matched by email.
func rosterAdded(current, merged calendar.Event, organizer string) bool {
	had := make(map[string]bool)
	for _, a := range eventRecipients(current, organizer) {
		had[strings.ToLower(a.Email)] = true
	}
	for _, a := range eventRecipients(merged, organizer) {
		if !had[strings.ToLower(a.Email)] {
			return true
		}
	}
	return false
}

// resetRecipientStatus marks every recipient attendee not-yet-responded — the
// neutral PARTSTAT=NEEDS-ACTION an organizer's significant change re-solicits, so
// the stored event shows responses are pending again.
func resetRecipientStatus(event *calendar.Event, organizer string) {
	eachAttendee(event, func(a *jscalParticipant) {
		if isSchedulingRecipient(*a, organizer) {
			a.ParticipationStatus = "needs-action"
		}
	})
}

// shouldSelfSchedule decides whether the server must send iMIP scheduling messages
// for event itself. It is true only with a scheduling provider, a known mailbox
// address, at least one attendee other than the organizer, and a backend that does
// NOT schedule natively (RFC 6638) — a native server does it itself, so we must not
// double-drive. organizer is the resolved mailbox address ("" when unavailable).
func (h Handler) shouldSelfSchedule(ctx context.Context, b calendar.Backend, event calendar.Event) (organizer string, send bool) {
	if h.scheduling == nil {
		return "", false
	}
	organizer, err := h.scheduling.MailboxAddress(ctx)
	if err != nil || organizer == "" {
		return "", false
	}
	hasRecipient := false
	for _, a := range event.Attendees() {
		if isSchedulingRecipient(a, organizer) {
			hasRecipient = true
			break
		}
	}
	if !hasRecipient {
		return organizer, false
	}
	if native, _ := serverSchedulesEvents(ctx, b); native {
		return organizer, false
	}
	return organizer, true
}

// isSchedulingRecipient reports whether an attendee should receive a scheduling
// message: a real address that is not the organizer's own.
func isSchedulingRecipient(a jscalParticipant, organizer string) bool {
	email := calendar.ParticipantEmail(a)
	return email != "" && !strings.EqualFold(email, organizer)
}

// schedulingRecipients keeps the attendees a scheduling message should go to (real
// addresses other than the organizer), dropping the organizer so we never mail them
// their own invitation or cancellation.
func schedulingRecipients(attendees []scheduling.Attendee, organizer string) []scheduling.Attendee {
	out := make([]scheduling.Attendee, 0, len(attendees))
	for _, a := range attendees {
		if a.Email != "" && !strings.EqualFold(a.Email, organizer) {
			out = append(out, a)
		}
	}
	return out
}

// organizerFor forces the scheduling organizer to the configured mailbox, keeping
// the display name the client supplied when they already named themselves as
// organizer (the CN is lost only when we synthesize the organizer, where no name
// is available).
func organizerFor(eventOrganizer scheduling.Address, mailbox string) scheduling.Address {
	if strings.EqualFold(eventOrganizer.Email, mailbox) {
		return eventOrganizer
	}
	return scheduling.Address{Email: mailbox}
}

// recordSchedulingOutcome stamps the configured mailbox as organizer (preserving a
// client-supplied display name) and writes the SCHEDULE-STATUS of the scheduling
// message just sent onto each recipient attendee, so the persisted event reflects
// delivery. Shared by the create (REQUEST) and update (re-invite) paths.
func recordSchedulingOutcome(event *calendar.Event, organizer string, status scheduling.ScheduleStatus) {
	if org, ok := event.Organizer(); !ok || !strings.EqualFold(calendar.ParticipantEmail(org), organizer) {
		setEventOrganizer(event, calendar.NewParticipant("", organizer, "", "owner"))
	}
	eachAttendee(event, func(a *jscalParticipant) {
		if isSchedulingRecipient(*a, organizer) {
			a.ScheduleStatus = []string{string(status)}
		}
	})
}

// reinviteOnUpdate carries a PATCH's scheduling consequences to a meeting's
// attendees on the dumb-backend tier and returns the event to persist:
//
//   - attendees the patch dropped are sent a METHOD:CANCEL (withdrawing the meeting
//     from their calendars);
//   - a start/end/location change OR an added attendee re-sends a METHOD:REQUEST to
//     the remaining attendees, bumping SEQUENCE so it supersedes the prior one;
//   - a start/end/location change additionally resets the recipients' PARTSTAT,
//     since a significant change (RFC 5546) requires them to respond afresh — an
//     added-attendee-only change leaves existing responses intact.
//
// It is gated by the capability switch (no-op for a native scheduler) but, unlike
// the create path, not on the presence of recipients: a removal must be cancelled
// even when none remain. The event is returned unchanged when nothing an attendee
// needs to hear about changed.
func (h Handler) reinviteOnUpdate(ctx context.Context, b calendar.Backend, current, merged calendar.Event) calendar.Event {
	if h.scheduling == nil {
		return merged
	}
	organizer, err := h.scheduling.MailboxAddress(ctx)
	if err != nil || organizer == "" {
		return merged
	}
	if native, _ := serverSchedulesEvents(ctx, b); native {
		return merged
	}

	removed := removedRecipients(current, merged, organizer)
	timeLocChanged := significantChange(current, merged)
	if len(removed) == 0 && !timeLocChanged && !rosterAdded(current, merged, organizer) {
		return merged
	}

	if len(removed) > 0 {
		// Cancel using the current (pre-patch) details — the meeting the removed
		// attendees still hold.
		h.sendCancelTo(ctx, organizer, current, removed)
	}

	if timeLocChanged || rosterAdded(current, merged, organizer) {
		merged.Sequence = current.Sequence + 1
		if timeLocChanged {
			resetRecipientStatus(&merged, organizer)
		}
		status := h.sendRequest(ctx, organizer, merged)
		recordSchedulingOutcome(&merged, organizer, status)
	} else {
		// A removal-only change: nothing to re-invite, but the event still advanced.
		merged.Sequence = current.Sequence + 1
	}
	return merged
}

// significantChange reports whether a PATCH altered a field that, per RFC 5546,
// warrants re-inviting attendees, bumping SEQUENCE, and resetting their PARTSTAT:
// the start, end, or location. A summary- or body-only edit is not significant. An
// attendee-set change is handled separately (see reinviteOnUpdate) and does not
// reset existing attendees' responses.
func significantChange(current, merged calendar.Event) bool {
	return !current.StartTime().Equal(merged.StartTime()) ||
		!current.EndTime().Equal(merged.EndTime()) ||
		eventLocation(current) != eventLocation(merged)
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

	// Organizer side: a significant change to a meeting re-sends the REQUEST (with a
	// bumped SEQUENCE) so attendees see the update — gated by the capability switch.
	merged = h.reinviteOnUpdate(ctx, b, current, merged)

	updated, err := h.updateEvent(ctx, b, w, merged, params.IfMatch)
	if err != nil {
		return nil, err
	}
	return &api.MicrosoftGraphEventStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphEvent(updated),
	}, nil
}

// updateEvent writes merged, honouring an inbound If-Match precondition when one
// is supplied and the backend supports conditional writes (CalDAV via a
// conditional PUT). A failed precondition surfaces calendar.ErrPreconditionFailed,
// which the error handler maps to 412. With no If-Match, or a backend that only
// implements the unconditional Writer, it writes unconditionally.
func (h Handler) updateEvent(ctx context.Context, b calendar.Backend, w calendar.Writer, merged calendar.Event, ifMatch api.OptString) (calendar.Event, error) {
	if etag, conditional := ifMatchOf(ifMatch); conditional {
		if cw, ok := b.(calendar.ConditionalWriter); ok {
			updated, err := cw.UpdateEventIfMatch(ctx, merged, etag)
			if err != nil {
				return calendar.Event{}, fmt.Errorf("update event (conditional): %w", err)
			}
			return updated, nil
		}
	}
	updated, err := w.UpdateEvent(ctx, merged)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("update event: %w", err)
	}
	return updated, nil
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

	// Read the event before deleting so its attendees can be sent a CANCEL. A read
	// failure is tolerated: the delete still proceeds, just without notification.
	event, getErr := b.GetEvent(ctx, params.EventID)

	if err := w.DeleteEvent(ctx, params.EventID); err != nil {
		return nil, fmt.Errorf("delete event: %w", err)
	}
	// Organizer side of the iTIP engine: withdraw the meeting from the attendees'
	// calendars. Best-effort — the delete has already succeeded.
	if getErr == nil {
		h.cancelInvitations(ctx, b, event)
	}
	return &api.MeDeleteEventsNoContent{}, nil
}

// graphToEvent maps the inbound Graph event onto the neutral calendar.Event — the
// inverse of toGraphEvent. Read-only and server-assigned fields (ID, ICalUId) are
// ignored: the backend stamps the created event with its own opaque ID/UID. Times
// are interpreted in their declared zone and stored as UTC instants (via
// SetUTCTimes); the embedded JSCalendar participants/locations/recurrence are built
// from the helpers in internal/calendar.
func graphToEvent(ge *api.MicrosoftGraphEvent) (calendar.Event, error) {
	var e calendar.Event
	e.Title = ge.Subject.Or("")
	e.ShowWithoutTime = ge.IsAllDay.Or(false)

	start, end, err := graphStartEnd(ge.Start, ge.End)
	if err != nil {
		return calendar.Event{}, err
	}
	e.SetUTCTimes(start, end)

	if v, ok := ge.Location.Get(); ok {
		setEventLocation(&e, v.DisplayName.Or(""))
	}
	if v, ok := ge.Body.Get(); ok {
		e.Description = v.Content.Or("")
		e.DescriptionContentType = neutralBodyType(v.ContentType)
	}
	if v, ok := ge.Recurrence.Get(); ok {
		rules, err := recurrenceFromGraph(v)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("recurrence: %w", err)
		}
		e.RecurrenceRules = rules
	}

	var organizer *jscalParticipant
	if v, ok := ge.Organizer.Get(); ok {
		if p, ok := graphRecipientToParticipant(v); ok {
			organizer = &p
		}
	}
	e.SetOrganizerAttendees(organizer, graphToAttendees(ge.Attendees))
	return e, nil
}

// graphStartEnd parses the optional Graph start/end dateTimeTimeZone pair to UTC
// instants. A zone the server cannot resolve is a 400-worthy error on either side.
func graphStartEnd(start, end api.OptMicrosoftGraphDateTimeTimeZone) (s, e time.Time, err error) {
	if v, ok := start.Get(); ok {
		if s, err = graphToTime(v); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
		}
	}
	if v, ok := end.Get(); ok {
		if e, err = graphToTime(v); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
		}
	}
	return s, e, nil
}

// setEventLocation stores a single display-name location under JSCalendar key "1"
// (the one eventLocation reads back), or clears the locations when name is empty.
func setEventLocation(e *calendar.Event, name string) {
	if name == "" {
		e.Locations = nil
		return
	}
	e.Locations = map[jscalendar.Id]jscalendar.Location{"1": {Name: name}}
}

// patchedZone reports the IANA zone a patched Graph Start carries, resolving
// Windows zone names (and treating ""/"UTC" as Etc/UTC), so a time PATCH that names
// a zone rebinds the event to it. ok is false when the dateTime is an RFC 3339
// instant (it fixes its own offset, carrying no named zone) or carries no zone.
func patchedZone(dt api.MicrosoftGraphDateTimeTimeZone) (jscalendar.TimeZoneId, bool) {
	s, ok := dt.DateTime.Get()
	if !ok || s == "" {
		return "", false
	}
	// An RFC 3339 instant has no named zone to preserve.
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return "", false
	}
	name, ok := dt.TimeZone.Get()
	if !ok || name == "" {
		return "", false
	}
	if strings.EqualFold(name, "UTC") {
		return "Etc/UTC", true
	}
	loc, err := tz.Lookup(name)
	if err != nil {
		return "", false
	}
	return jscalendar.TimeZoneId(loc.String()), true
}

// setTimesInZone sets the event's Start (as a wall-clock LocalDateTime in zone),
// TimeZone, and Duration from a pair of absolute UTC instants — the zone-preserving
// counterpart to the frozen SetUTCTimes (which always stamps Etc/UTC). An empty or
// unresolvable zone falls back to SetUTCTimes' UTC behavior.
func setTimesInZone(e *calendar.Event, start, end time.Time, zone jscalendar.TimeZoneId) {
	if zone == "" || zone == "Etc/UTC" {
		e.SetUTCTimes(start, end)
		return
	}
	loc, err := time.LoadLocation(string(zone))
	if err != nil {
		e.SetUTCTimes(start, end)
		return
	}
	if start.IsZero() {
		e.Start = nil
		e.TimeZone = ""
		e.Duration = nil
		return
	}
	local := start.In(loc)
	ldt := jscalendar.LocalDateTime{
		Year: local.Year(), Month: int(local.Month()), Day: local.Day(),
		Hour: local.Hour(), Minute: local.Minute(), Second: local.Second(),
	}
	e.Start = &ldt
	e.TimeZone = zone
	if !end.IsZero() && end.After(start) {
		e.Duration = durationToJSCal(end.Sub(start))
	} else {
		e.Duration = nil
	}
}

// durationToJSCal expresses a positive Go duration as a JSCalendar Duration in
// hours/minutes/seconds, matching the frozen SetUTCTimes emission. A non-positive
// duration yields nil (default zero-length).
func durationToJSCal(d time.Duration) *jscalendar.Duration {
	if d <= 0 {
		return nil
	}
	return &jscalendar.Duration{
		Hours:   uint64(d / time.Hour),
		Minutes: uint64((d % time.Hour) / time.Minute),
		Seconds: uint64((d % time.Minute) / time.Second),
	}
}

// setEventOrganizer replaces the event's organizer (the "owner"-role participant)
// while preserving the attendee roster, rebuilding the Participants map through the
// frozen helper so keying stays deterministic.
func setEventOrganizer(e *calendar.Event, organizer jscalParticipant) {
	org := organizer
	e.SetOrganizerAttendees(&org, e.Attendees())
}

// setEventAttendees replaces the event's attendee roster while preserving its
// organizer, rebuilding the Participants map deterministically.
func setEventAttendees(e *calendar.Event, attendees []jscalParticipant) {
	var org *jscalParticipant
	if o, ok := e.Organizer(); ok {
		org = &o
	}
	e.SetOrganizerAttendees(org, attendees)
}

// eachAttendee applies fn to a mutable copy of every attendee participant and
// writes the result back, leaving the organizer untouched. It is the in-place
// mutation primitive for the scheduling status/SCHEDULE-STATUS bookkeeping that the
// bespoke model did by indexing into a slice.
func eachAttendee(e *calendar.Event, fn func(*jscalParticipant)) {
	attendees := e.Attendees()
	for i := range attendees {
		fn(&attendees[i])
	}
	setEventAttendees(e, attendees)
}

// scheduleStatusValue returns a participant's single SCHEDULE-STATUS code (the
// neutral model carries the RFC 6638 codes as a list), or "" when none is set.
func scheduleStatusValue(p jscalParticipant) string {
	if len(p.ScheduleStatus) == 0 {
		return ""
	}
	return p.ScheduleStatus[0]
}

// mergeEventPatch overlays the fields present in the inbound Graph PATCH body
// onto the current event, leaving absent fields unchanged — the read-modify-write
// half of PATCH semantics. Presence is detected per field: scalar Opt/OptNil
// fields via .Get() (a set field overlays, even when its value is empty), and the
// Attendees collection via a non-empty slice. The event's identity (ID, UID, and
// the rest of the current record) is preserved so UpdateEvent rewrites in place.
// A patched Start/End naming an *unresolvable* time zone is rejected with a 400,
// just like create; a resolvable named zone is honored and preserved.
func mergeEventPatch(current calendar.Event, ge *api.MicrosoftGraphEvent) (calendar.Event, error) {
	merged := current
	if v, ok := ge.Subject.Get(); ok {
		merged.Title = v
	}
	if v, ok := ge.IsAllDay.Get(); ok {
		merged.ShowWithoutTime = v
	}
	// Start and End are coupled in the neutral model (End is Start + Duration), so a
	// patch to either is applied by recomputing the pair from absolute instants: the
	// patched side overlays the current instant. The event's named TimeZone is
	// PRESERVED (the MB720-49 fidelity goal) — a time-only edit must not silently
	// relabel an America/New_York event to UTC — unless the patched Start itself
	// carries a different zone, in which case that zone wins. Only re-apply when the
	// patch actually touches a time field, to leave the stored times untouched
	// otherwise.
	if _, hasStart := ge.Start.Get(); hasStart || ge.End.Set {
		start, end := merged.StartTime(), merged.EndTime()
		zone := merged.TimeZone
		if v, ok := ge.Start.Get(); ok {
			t, err := graphToTime(v)
			if err != nil {
				return calendar.Event{}, fmt.Errorf("start: %w", err)
			}
			start = t
			if z, ok := patchedZone(v); ok {
				zone = z
			}
		}
		if v, ok := ge.End.Get(); ok {
			t, err := graphToTime(v)
			if err != nil {
				return calendar.Event{}, fmt.Errorf("end: %w", err)
			}
			end = t
		}
		setTimesInZone(&merged, start, end, zone)
	}
	if v, ok := ge.Location.Get(); ok {
		setEventLocation(&merged, v.DisplayName.Or(""))
	}
	if v, ok := ge.Body.Get(); ok {
		merged.Description = v.Content.Or("")
		merged.DescriptionContentType = neutralBodyType(v.ContentType)
	}
	if v, ok := ge.Organizer.Get(); ok {
		if p, ok := graphRecipientToParticipant(v); ok {
			setEventOrganizer(&merged, p)
		}
	}
	if len(ge.Attendees) > 0 {
		setEventAttendees(&merged, mergeAttendees(current.Attendees(), graphToAttendees(ge.Attendees)))
	}
	return merged, nil
}

// mergeAttendees overlays a PATCH's attendee roster onto the current one, carrying
// each retained attendee's stored response (and SCHEDULE-STATUS) forward when the
// patch does not restate it — a Graph client that re-lists attendees by address
// alone should not silently drop their existing PARTSTAT. Attendees only on the
// patch are added; those only on the current roster are dropped (a removal).
func mergeAttendees(current, patched []jscalParticipant) []jscalParticipant {
	byEmail := make(map[string]jscalParticipant, len(current))
	for _, a := range current {
		byEmail[strings.ToLower(calendar.ParticipantEmail(a))] = a
	}
	out := make([]jscalParticipant, 0, len(patched))
	for _, p := range patched {
		if prev, ok := byEmail[strings.ToLower(calendar.ParticipantEmail(p))]; ok {
			// Carry the stored response forward only when the patch omits it; but
			// SCHEDULE-STATUS has no Graph representation, so a patch can never
			// restate it — always preserve the stored value.
			if p.ParticipationStatus == "" {
				p.ParticipationStatus = prev.ParticipationStatus
			}
			if len(p.ScheduleStatus) == 0 {
				p.ScheduleStatus = prev.ScheduleStatus
			}
		}
		out = append(out, p)
	}
	return out
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

// graphRecipientToParticipant maps a Graph recipient onto a JSCalendar owner
// participant (the organizer), reporting ok=false when the recipient carries no
// email address.
func graphRecipientToParticipant(r api.MicrosoftGraphRecipient) (jscalParticipant, bool) {
	ea, ok := r.EmailAddress.Get()
	if !ok {
		return jscalParticipant{}, false
	}
	email := ea.Address.Or("")
	if email == "" {
		return jscalParticipant{}, false
	}
	return calendar.NewParticipant(ea.Name.Or(""), email, "", "owner"), true
}

// graphToAttendees maps Graph attendees onto JSCalendar attendee participants,
// translating the Graph responseStatus.response onto the participationStatus.
func graphToAttendees(as []api.MicrosoftGraphAttendee) []jscalParticipant {
	if len(as) == 0 {
		return nil
	}
	out := make([]jscalParticipant, 0, len(as))
	for _, a := range as {
		ea, ok := a.EmailAddress.Get()
		if !ok {
			continue
		}
		partStat := ""
		if rs, ok := a.Status.Get(); ok {
			if rt, ok := rs.Response.Get(); ok {
				partStat = calendar.ResponseToPartStat(string(rt))
			}
		}
		out = append(out, calendar.NewParticipant(ea.Name.Or(""), ea.Address.Or(""), partStat, "attendee"))
	}
	return out
}

// neutralBodyType maps a Graph bodyType onto the JSCalendar descriptionContentType
// media type ("text/html" / "text/plain") — the inverse of graphBodyType.
func neutralBodyType(bt api.OptMicrosoftGraphBodyType) string {
	if v, ok := bt.Get(); ok && v == api.MicrosoftGraphBodyTypeHTML {
		return "text/html"
	}
	return "text/plain"
}
