package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// MeSettingsStorageGetQuota implements GET /me/settings/storage/quota: the mailbox's
// storage usage as a Graph unifiedStorageQuota, backed by the IMAP QUOTA extension
// (RFC 9208) when the mail backend supports it. A backend without quota support — or
// a mailbox the server reports no storage quota for — yields notImplemented.
func (h Handler) MeSettingsStorageGetQuota(ctx context.Context, _ api.MeSettingsStorageGetQuotaParams) (api.MeSettingsStorageGetQuotaRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	qr, ok := b.(mail.QuotaReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	q, err := qr.Quota(ctx)
	if err != nil {
		if errors.Is(err, mail.ErrNoQuota) {
			return nil, ht.ErrNotImplemented
		}
		return nil, fmt.Errorf("quota: %w", err)
	}

	doc := api.MicrosoftGraphUnifiedStorageQuota{
		Used:  api.NewOptNilInt64(q.Used),
		Total: api.NewOptNilInt64(q.Total),
	}
	if q.Total > 0 {
		doc.Remaining = api.NewOptNilInt64(q.Total - q.Used)
	}
	return &api.MicrosoftGraphUnifiedStorageQuotaStatusCode{
		StatusCode: http.StatusOK,
		Response:   doc,
	}, nil
}
