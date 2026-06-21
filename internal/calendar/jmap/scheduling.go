package jmap

import (
	"context"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// SupportsServerScheduling reports whether the JMAP server implements iTIP
// scheduling. A JMAP server that advertises the calendars capability
// (urn:ietf:params:jmap:calendars) automatically supports server-side iTIP
// scheduling (RFC 6638), so we check for the presence of that capability in
// the session.
func (cl *Client) SupportsServerScheduling(ctx context.Context) (bool, error) {
	_, ok := cl.c.Session.RawCapabilities[calendarsURI]
	return ok, nil
}

// Verify that Client implements the SchedulingDetector interface.
var _ calendar.SchedulingDetector = (*Client)(nil)
