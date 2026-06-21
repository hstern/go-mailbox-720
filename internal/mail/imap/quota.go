package imap

import (
	"context"
	"fmt"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

var _ mail.QuotaReader = (*Client)(nil)

// Quota reports the mailbox's storage usage via the IMAP QUOTA extension
// (RFC 9208): GETQUOTAROOT on INBOX, then the STORAGE resource of the first quota
// root that advertises one. IMAP reports STORAGE in units of 1024 octets, so the
// usage and limit are scaled to bytes. It returns mail.ErrNoQuota when no quota
// root exposes a STORAGE resource (the server tracks no storage quota).
func (cl *Client) Quota(_ context.Context) (mail.Quota, error) {
	roots, err := cl.c.GetQuotaRoot("INBOX").Wait()
	if err != nil {
		return mail.Quota{}, fmt.Errorf("imap getquotaroot: %w", err)
	}
	for _, root := range roots {
		if res, ok := root.Resources[goimap.QuotaResourceStorage]; ok {
			const kib = 1024 // RFC 9208: the STORAGE resource is in units of 1024 octets.
			return mail.Quota{Used: res.Usage * kib, Total: res.Limit * kib}, nil
		}
	}
	return mail.Quota{}, mail.ErrNoQuota
}
