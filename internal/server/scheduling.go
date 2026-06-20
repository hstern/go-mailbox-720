package server

import (
	"context"
	"fmt"
	"time"

	ht "github.com/ogen-go/ogen/http"

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
// the event, and (unless the client set sendResponse=false) emails the organizer
// an iMIP REPLY conveying partStat, from the configured mailbox address.
//
// It returns (nil, nil) on success (the caller answers 204); (nil, error) for a
// 501/500 (no scheduling provider, or a backend failure); or (*ErrorStatusCode,
// nil) for a 400 (e.g. no mailbox address configured to reply as). Updating the
// stored event's response status is future work — this sends the reply.
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
	if !sendResponse.Or(true) {
		return nil, nil
	}

	me := h.scheduling.MailboxAddress()
	if me == "" {
		return badRequest("no mailbox address is configured, so a meeting response cannot be sent"), nil
	}

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
