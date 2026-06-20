package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// MeMessagesDelta implements GET /me/messages/delta — incremental sync of the
// inbox. It type-asserts the mail backend to mail.DeltaReader (a capability the
// IMAP adapter provides; a backend without it yields 501) and asks for the
// messages changed since the opaque continuation token the client echoed back
// from a prior @odata.deltaLink. The returned next token is folded into this
// response's @odata.deltaLink, which the client follows for the next round.
//
// This first cut is additive (new messages by UID); reporting deletions and
// flag/read-state changes needs IMAP CONDSTORE/QRESYNC and is future work.
func (h Handler) MeMessagesDelta(ctx context.Context, params api.MeMessagesDeltaParams) (api.MeMessagesDeltaRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	d, ok := b.(mail.DeltaReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	// $deltatoken carries our opaque sync token for the next round; $skiptoken is
	// the paging variant. Either (or neither, for the initial sync) is accepted.
	token := params.Deltatoken.Or(params.Skiptoken.Or(""))

	msgs, next, err := d.Delta(ctx, "", token)
	if err != nil {
		// A bad continuation token is client error: tell the client to resync
		// (drop the token and start over) rather than 500 a request that can
		// never succeed.
		if errors.Is(err, mail.ErrInvalidDeltaToken) {
			return resyncRequired(), nil
		}
		// The backing IMAP server lacks CONDSTORE, so delta cannot be served:
		// surface 501 so the operator learns the server is unsuitable.
		if errors.Is(err, mail.ErrDeltaUnsupported) {
			return nil, ht.ErrNotImplemented
		}
		return nil, fmt.Errorf("messages delta: %w", err)
	}

	value := make([]api.MicrosoftGraphMessage, 0, len(msgs))
	for _, m := range msgs {
		value = append(value, toGraphMessage(m))
	}
	return &api.MeMessagesDelta2XXStatusCode{
		StatusCode: http.StatusOK,
		Response: api.MeMessagesDelta2XX{
			Value:             value,
			OdataDotDeltaLink: api.NewOptNilString(deltaLink("/me/messages/delta()", next)),
		},
	}, nil
}

// deltaLink builds the @odata.deltaLink a client GETs for the next sync round:
// the given delta operation path under the API base prefix, carrying the opaque
// continuation token.
func deltaLink(opPath, token string) string {
	return basePath + opPath + "?$deltatoken=" + url.QueryEscape(token)
}
