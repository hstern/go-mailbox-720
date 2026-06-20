package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/itip"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

// SchedulingProvider supplies what answering a meeting invitation needs: an SMTP
// sender for the outbound iMIP REPLY, and the mailbox owner's address to use as
// the responding attendee. It is separate from the read/write calendar provider
// because replying is a mail action (it emails the organizer), not a CalDAV one.
// The static implementation lives in cmd/mailboxd.
type SchedulingProvider interface {
	// Sender returns an SMTP sender for one reply; the caller closes it.
	Sender(ctx context.Context) (smtp.Sender, error)
	// MailboxAddress is the mailbox owner's email, the responding attendee on a
	// reply. An empty value means the responder is unknown and replies are refused.
	MailboxAddress() string
}

// MeEventsEventAccept implements POST /me/events/{event-id}/accept: it emails an
// ACCEPTED iMIP reply to the event's organizer.
func (h Handler) MeEventsEventAccept(ctx context.Context, req *api.MeEventsEventAcceptReq, params api.MeEventsEventAcceptParams) (api.MeEventsEventAcceptRes, error) {
	errRes, err := h.respondToInvite(ctx, params.EventID, req.SendResponse, scheduling.PartStatAccepted)
	if err != nil {
		return nil, err
	}
	if errRes != nil {
		return errRes, nil
	}
	return &api.MeEventsEventAcceptNoContent{}, nil
}

// MeEventsEventDecline implements POST /me/events/{event-id}/decline.
func (h Handler) MeEventsEventDecline(ctx context.Context, req *api.MeEventsEventDeclineReq, params api.MeEventsEventDeclineParams) (api.MeEventsEventDeclineRes, error) {
	errRes, err := h.respondToInvite(ctx, params.EventID, req.SendResponse, scheduling.PartStatDeclined)
	if err != nil {
		return nil, err
	}
	if errRes != nil {
		return errRes, nil
	}
	return &api.MeEventsEventDeclineNoContent{}, nil
}

// MeEventsEventTentativelyAccept implements POST /me/events/{event-id}/tentativelyAccept.
func (h Handler) MeEventsEventTentativelyAccept(ctx context.Context, req *api.MeEventsEventTentativelyAcceptReq, params api.MeEventsEventTentativelyAcceptParams) (api.MeEventsEventTentativelyAcceptRes, error) {
	errRes, err := h.respondToInvite(ctx, params.EventID, req.SendResponse, scheduling.PartStatTentative)
	if err != nil {
		return nil, err
	}
	if errRes != nil {
		return errRes, nil
	}
	return &api.MeEventsEventTentativelyAcceptNoContent{}, nil
}

// respondToInvite is the shared accept/decline/tentativelyAccept body: it reads
// the event and conveys partStat to the organizer. How depends on the RFC 6638
// capability switch — when the CalDAV server schedules natively, it records the
// response as the responder's PARTSTAT (UpdateEvent) and lets the server send the
// iTIP reply; otherwise it emails the reply itself.
//
// It returns (nil, nil) on success (the caller answers 204); (nil, error) for a
// 501/500 (no scheduling provider, a read-only backend on a native server, or a
// backend failure); or (*ErrorStatusCode, nil) for a 400 (no mailbox address, or
// the mailbox is not an attendee).
func (h Handler) respondToInvite(ctx context.Context, eventID string, sendResponse api.OptNilBool, partStat scheduling.PartStat) (*api.ErrorStatusCode, error) {
	if h.scheduling == nil {
		return nil, ht.ErrNotImplemented
	}

	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	event, err := b.GetEvent(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("get event: %w", err)
	}

	// Graph's sendResponse defaults to true; only an explicit false suppresses the
	// reply (the user records their status without notifying the organizer).
	//
	// NOTE: the generated request type carries a spec-level `default: false` that
	// ogen applies on decode, so an OMITTED sendResponse currently arrives as
	// Set=true/Value=false and is treated as false here — meaning a client that
	// omits the field gets no reply. Correcting that needs the subsetter to strip
	// the default (tracked separately); a client can force the reply with an
	// explicit sendResponse:true.
	if !sendResponse.Or(true) {
		return nil, nil
	}

	me := h.scheduling.MailboxAddress()
	if me == "" {
		return badRequest("no mailbox address is configured, so a meeting response cannot be sent"), nil
	}

	// RFC 6638 capability switch: a server that schedules natively sends the iTIP
	// reply itself when the responder's PARTSTAT changes, so record the response
	// via CalDAV rather than emailing — which would otherwise double up with the
	// server's own reply.
	if native, _ := serverSchedulesEvents(ctx, b); native {
		w, ok := b.(calendar.Writer)
		if !ok {
			return nil, ht.ErrNotImplemented
		}
		if !setAttendeeStatus(&event, me, partStatToNeutral(partStat)) {
			return badRequest("the configured mailbox is not an attendee of this event"), nil
		}
		if _, err := w.UpdateEvent(ctx, event); err != nil {
			return nil, fmt.Errorf("update response status: %w", err)
		}
		return nil, nil
	}

	// Storage-only server: email the iMIP reply ourselves.
	sender, err := h.scheduling.Sender(ctx)
	if err != nil {
		return nil, fmt.Errorf("smtp sender: %w", err)
	}
	defer func() { _ = sender.Close() }()

	if err := itip.RespondToEvent(ctx, sender, event, scheduling.Address{Email: me}, partStat, time.Now()); err != nil {
		return nil, fmt.Errorf("respond to event: %w", err)
	}
	return nil, nil
}

// serverSchedulesEvents reports whether the calendar backend's server performs
// RFC 6638 auto-scheduling. A backend that cannot detect it (no SchedulingDetector)
// is treated as storage-only.
func serverSchedulesEvents(ctx context.Context, b calendar.Backend) (bool, error) {
	d, ok := b.(calendar.SchedulingDetector)
	if !ok {
		return false, nil
	}
	return d.SupportsServerScheduling(ctx)
}

// setAttendeeStatus sets the participation status of the attendee whose email
// matches (case-insensitively), reporting whether one was found.
func setAttendeeStatus(event *calendar.Event, email, status string) bool {
	for i := range event.Attendees {
		if strings.EqualFold(event.Attendees[i].Email, email) {
			event.Attendees[i].Status = status
			return true
		}
	}
	return false
}

// partStatToNeutral maps a scheduling PARTSTAT to the neutral attendee status.
func partStatToNeutral(p scheduling.PartStat) string {
	switch p {
	case scheduling.PartStatAccepted:
		return "accepted"
	case scheduling.PartStatDeclined:
		return "declined"
	case scheduling.PartStatTentative:
		return "tentativelyAccepted"
	default:
		return "notResponded"
	}
}
