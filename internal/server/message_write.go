package server

import (
	"context"
	"fmt"
	"net/http"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// MeUpdateMessages implements PATCH /me/messages/{message-id}. The only mutable
// message property the mail Writer exposes is the read state, so this honours the
// inbound body's isRead and calls mail.Writer.SetRead; other properties in the
// body are ignored. It returns the (now-current) message, mapped from a fresh
// read, with 200 OK. The backend is obtained via backend (nil-provider -> 501)
// and type-asserted to mail.Writer; a read-only backend yields 501.
//
// Deferred: MeCreateMessages (draft creation) — the mail Writer has no create
// capability, so it is left UnimplementedHandler for a follow-up.
func (h Handler) MeUpdateMessages(ctx context.Context, req *api.MicrosoftGraphMessage, params api.MeUpdateMessagesParams) (api.MeUpdateMessagesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(mail.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	if read, ok := req.IsRead.Get(); ok {
		if err := w.SetRead(ctx, params.MessageID, read); err != nil {
			return nil, fmt.Errorf("set read: %w", err)
		}
	}

	m, err := b.GetMessage(ctx, params.MessageID)
	if err != nil {
		return nil, fmt.Errorf("get message: %w", err)
	}
	return &api.MicrosoftGraphMessageStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphMessage(m),
	}, nil
}

// MeDeleteMessages implements DELETE /me/messages/{message-id}. It type-asserts
// the backend to mail.Writer (read-only backend -> 501) and deletes the message,
// returning 204 No Content.
func (h Handler) MeDeleteMessages(ctx context.Context, params api.MeDeleteMessagesParams) (api.MeDeleteMessagesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(mail.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	if err := w.DeleteMessage(ctx, params.MessageID); err != nil {
		return nil, fmt.Errorf("delete message: %w", err)
	}
	return &api.MeDeleteMessagesNoContent{}, nil
}
