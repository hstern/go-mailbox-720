package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// messageFilterFields is the allow-list of message properties a $filter on
// /me/messages may reference. A filter that names anything else is rejected with
// a Graph 400 before it reaches the backend.
var messageFilterFields = []string{
	"subject",
	"from",
	"from/emailAddress/address",
	"to",
	"to/emailAddress/address",
	"receivedDateTime",
	"isRead",
	"hasAttachments",
}

// MailProvider yields a mail.Backend for an authenticated request. The static
// implementation lives in cmd/mailboxd; per-identity providers (mapping the
// token's mailbox identity to backend credentials) come later.
type MailProvider interface {
	Mail(ctx context.Context) (mail.Backend, error)
}

// backend resolves the request's mail backend, or reports "not implemented" when
// no provider is configured (the skeleton posture).
func (h Handler) backend(ctx context.Context) (mail.Backend, error) {
	if h.mail == nil {
		return nil, ht.ErrNotImplemented
	}
	return h.mail.Mail(ctx)
}

// MeListMessages implements GET /me/messages by listing the inbox. A $filter
// query option, if present, is parsed and validated against the supported
// message fields; a malformed or unsupported filter returns a Graph 400 without
// touching the backend.
func (h Handler) MeListMessages(ctx context.Context, params api.MeListMessagesParams) (api.MeListMessagesRes, error) {
	filter, errRes := parseMessageFilter(params.Filter)
	if errRes != nil {
		return errRes, nil
	}

	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	msgs, err := b.ListMessages(ctx, "", mail.Page{Top: params.Top.Or(0), Skip: params.Skip.Or(0)}, filter)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	value := make([]api.MicrosoftGraphMessage, 0, len(msgs))
	for _, m := range msgs {
		value = append(value, toGraphMessage(m))
	}
	return &api.MicrosoftGraphMessageCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphMessageCollectionResponse{Value: value},
	}, nil
}

// MeGetMessages implements GET /me/messages/{message-id}.
func (h Handler) MeGetMessages(ctx context.Context, params api.MeGetMessagesParams) (api.MeGetMessagesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	m, err := b.GetMessage(ctx, params.MessageID)
	if err != nil {
		return nil, fmt.Errorf("get message: %w", err)
	}
	return &api.MicrosoftGraphMessageStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphMessage(m),
	}, nil
}

// MeListMailFolders implements GET /me/mailFolders.
func (h Handler) MeListMailFolders(ctx context.Context, _ api.MeListMailFoldersParams) (api.MeListMailFoldersRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	folders, err := b.ListMailFolders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list mail folders: %w", err)
	}
	value := make([]api.MicrosoftGraphMailFolder, 0, len(folders))
	for _, f := range folders {
		value = append(value, toGraphFolder(f))
	}
	return &api.MicrosoftGraphMailFolderCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphMailFolderCollectionResponse{Value: value},
	}, nil
}

// MeGetMailFolders implements GET /me/mailFolders/{mailFolder-id}. The id may be
// an opaque folder id or one of Graph's well-known folder name aliases (inbox,
// sentitems, drafts, deleteditems, junkemail, archive), which resolves to the
// backend's special-use folder of that role (case-insensitively, as Graph
// treats these aliases). It returns 404 when no folder matches.
func (h Handler) MeGetMailFolders(ctx context.Context, params api.MeGetMailFoldersParams) (api.MeGetMailFoldersRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	folders, err := b.ListMailFolders(ctx)
	if err != nil {
		return nil, fmt.Errorf("get mail folder: %w", err)
	}
	id := params.MailFolderID
	for _, f := range folders {
		if f.ID == id || (f.WellKnownName != "" && strings.EqualFold(f.WellKnownName, id)) {
			return &api.MicrosoftGraphMailFolderStatusCode{
				StatusCode: http.StatusOK,
				Response:   toGraphFolder(f),
			}, nil
		}
	}
	return notFound("mail folder not found"), nil
}
