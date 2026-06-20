package caldav

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

var _ calendar.SchedulingDetector = (*Client)(nil)

// capAutoSchedule is the RFC 6638 compliance class a server advertises in its DAV
// response header when it implements calendar auto-scheduling (it performs iTIP
// itself via scheduling inbox/outbox collections).
const capAutoSchedule = "calendar-auto-schedule"

// SupportsServerScheduling reports whether the CalDAV server implements RFC 6638
// calendar auto-scheduling, detected by an OPTIONS request whose DAV response
// header lists calendar-auto-schedule. When true the server handles iTIP itself,
// so the client-side email scheduling bridge should stand down.
//
// go-webdav parses the DAV header internally but does not surface it through the
// caldav client, so the adapter issues this one OPTIONS request directly over its
// authenticated transport.
func (cl *Client) SupportsServerScheduling(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, cl.endpoint.String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := cl.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("caldav: options: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The DAV header is a comma-separated set of compliance classes, e.g.
	// "1, 2, 3, calendar-access, calendar-auto-schedule".
	for _, header := range resp.Header.Values("DAV") {
		for _, class := range strings.Split(header, ",") {
			if strings.EqualFold(strings.TrimSpace(class), capAutoSchedule) {
				return true, nil
			}
		}
	}
	return false, nil
}
