package server

import (
	"context"
	"errors"
	"testing"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/odata"
)

// bareMailBackend is a mail.Backend with no optional capabilities (no QuotaReader).
type bareMailBackend struct{}

func (bareMailBackend) ListMailFolders(context.Context) ([]mail.MailFolder, error) { return nil, nil }
func (bareMailBackend) ListMessages(context.Context, string, mail.Page, *odata.Filter) ([]mail.Message, error) {
	return nil, nil
}
func (bareMailBackend) GetMessage(context.Context, string) (mail.Message, error) {
	return mail.Message{}, nil
}
func (bareMailBackend) Close() error { return nil }

// quotaMailBackend adds the QuotaReader capability to the bare backend.
type quotaMailBackend struct {
	bareMailBackend
	q       mail.Quota
	noQuota bool
}

func (b quotaMailBackend) Quota(context.Context) (mail.Quota, error) {
	if b.noQuota {
		return mail.Quota{}, mail.ErrNoQuota
	}
	return b.q, nil
}

type mailProviderFunc func(context.Context) (mail.Backend, error)

func (f mailProviderFunc) Mail(ctx context.Context) (mail.Backend, error) { return f(ctx) }

func quotaTestHandler(b mail.Backend) Handler {
	return Handler{mail: mailProviderFunc(func(context.Context) (mail.Backend, error) { return b, nil })}
}

func TestMeSettingsStorageGetQuota(t *testing.T) {
	const mib = 1024 * 1024
	h := quotaTestHandler(quotaMailBackend{q: mail.Quota{Used: 2 * mib, Total: 10 * mib}})

	res, err := h.MeSettingsStorageGetQuota(context.Background(), api.MeSettingsStorageGetQuotaParams{})
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphUnifiedStorageQuotaStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want unifiedStorageQuota", res)
	}
	q := ok.Response
	if v, _ := q.Used.Get(); v != 2*mib {
		t.Errorf("used = %d, want %d", v, 2*mib)
	}
	if v, _ := q.Total.Get(); v != 10*mib {
		t.Errorf("total = %d, want %d", v, 10*mib)
	}
	if v, _ := q.Remaining.Get(); v != 8*mib {
		t.Errorf("remaining = %d, want %d", v, 8*mib)
	}
}

func TestMeSettingsStorageGetQuotaNotImplemented(t *testing.T) {
	// A backend without the QuotaReader capability.
	if _, err := quotaTestHandler(bareMailBackend{}).MeSettingsStorageGetQuota(context.Background(), api.MeSettingsStorageGetQuotaParams{}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("no-capability err = %v, want ErrNotImplemented", err)
	}
	// A backend that reports no storage quota for the mailbox.
	if _, err := quotaTestHandler(quotaMailBackend{noQuota: true}).MeSettingsStorageGetQuota(context.Background(), api.MeSettingsStorageGetQuotaParams{}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("ErrNoQuota err = %v, want ErrNotImplemented", err)
	}
}
