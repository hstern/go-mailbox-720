package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/emersion/go-ical"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

var _ calendar.DeltaReader = (*Client)(nil)

// Delta performs an RFC 6578 sync-collection REPORT against the calendar
// collection and maps the changed calendar objects to neutral Events. An empty
// token is the initial sync: the server returns every current object plus a
// fresh sync-token. A non-empty token is an incremental sync: the server
// returns only the objects created or modified since that token, plus the next
// token to feed back on the following call.
//
// go-webdav v0.7.0's caldav Client exposes no SyncCollection (only carddav
// does) and keeps its internal HTTP client unexported, so this issues the
// REPORT itself against the same authenticated transport the adapter dialed
// with. The request asks for calendar-data inline (a VEVENT comp filter,
// mirroring the read path) so the changed objects arrive with their iCalendar
// bodies and map through the same eventFromObject/mapEvent path as ListEvents —
// each Event stamped with the opaque event id encoding its href.
//
// This first cut is additive: removed resources come back as <response>s with a
// 404 status and no calendar-data; they are skipped here. Surfacing them as
// tombstones is future work.
func (cl *Client) Delta(ctx context.Context, calID string, token string) ([]calendar.Event, string, error) {
	calPath, err := decodeCalendarID(calID)
	if err != nil {
		return nil, "", err
	}

	ms, err := cl.syncCollection(ctx, calPath, token)
	if err != nil {
		return nil, "", fmt.Errorf("caldav: delta: %w", err)
	}

	var events []calendar.Event
	for _, resp := range ms.Responses {
		href := resp.href()
		if href == "" || strings.TrimRight(href, "/") == strings.TrimRight(calPath, "/") {
			// The collection itself can appear in the response; it is not an event.
			continue
		}
		data := resp.calendarData()
		if data == "" {
			// A removed resource (404) or one without inlined data. Ignoring
			// removals is the documented first-cut limit; nothing to map here.
			continue
		}
		cal, err := ical.NewDecoder(strings.NewReader(data)).Decode()
		if err != nil {
			// A single unparseable object is skipped rather than failing the
			// whole sync, matching ListEvents' best-effort mapping.
			continue
		}
		if e, ok := eventFromObject(calID, href, cal); ok {
			events = append(events, e)
		}
	}

	return events, ms.SyncToken, nil
}

// syncCollection issues the sync-collection REPORT and decodes the multistatus.
func (cl *Client) syncCollection(ctx context.Context, calPath, token string) (*multiStatus, error) {
	body, err := xml.Marshal(syncCollectionQuery{
		SyncToken: token,
		SyncLevel: "1",
		Prop:      newSyncProp(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal sync-collection: %w", err)
	}

	u := *cl.endpoint
	u.Path = calPath
	req, err := http.NewRequestWithContext(ctx, "REPORT", u.String(),
		bytes.NewReader(append([]byte(xml.Header), body...)))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")

	resp, err := cl.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("report %q: %w", calPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMultiStatus {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("report %q: status %s: %s", calPath, resp.Status, bytes.TrimSpace(b))
	}

	var ms multiStatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode multistatus: %w", err)
	}
	return &ms, nil
}

// The XML types below model just enough of the DAV: sync-collection request and
// multistatus response (RFC 6578 / RFC 4791) to drive Delta. go-webdav's own
// equivalents live in its internal package, which is not importable, so the
// adapter declares its own minimal shapes here.

const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
)

// syncCollectionQuery is the request body: a sync-token (empty for the initial
// sync), the sync-level, and the props to return for each changed resource —
// here getetag plus the CalDAV calendar-data so objects arrive with their
// iCalendar bodies.
type syncCollectionQuery struct {
	XMLName   xml.Name `xml:"DAV: sync-collection"`
	SyncToken string   `xml:"DAV: sync-token"`
	SyncLevel string   `xml:"DAV: sync-level"`
	Prop      syncProp `xml:"DAV: prop"`
}

type syncProp struct {
	XMLName      xml.Name      `xml:"DAV: prop"`
	GetETag      *xml.Name     `xml:"DAV: getetag"`
	CalendarData *calendarData `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
}

// calendarData is the CalDAV calendar-data element. As a request prop it is an
// empty element; as a response prop its chardata carries the iCalendar object.
type calendarData struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	Data    string   `xml:",chardata"`
}

func newSyncProp() syncProp {
	return syncProp{
		GetETag:      &xml.Name{Space: nsDAV, Local: "getetag"},
		CalendarData: &calendarData{},
	}
}

// multiStatus models the sync-collection response: the per-resource responses
// plus the next sync-token to round-trip.
type multiStatus struct {
	XMLName   xml.Name       `xml:"DAV: multistatus"`
	Responses []syncResponse `xml:"DAV: response"`
	SyncToken string         `xml:"DAV: sync-token"`
}

type syncResponse struct {
	XMLName   xml.Name       `xml:"DAV: response"`
	Href      string         `xml:"DAV: href"`
	Status    string         `xml:"DAV: status"`
	PropStats []syncPropStat `xml:"DAV: propstat"`
}

type syncPropStat struct {
	Status string   `xml:"DAV: status"`
	Prop   syncProp `xml:"DAV: prop"`
}

// href returns the resource path of a response.
func (r syncResponse) href() string {
	return strings.TrimSpace(r.Href)
}

// calendarData returns the inlined iCalendar body for a response, taking it only
// from a 2xx propstat so a removed resource (404) yields "". A returned non-empty
// string is a calendar object ready to decode.
func (r syncResponse) calendarData() string {
	if r.Status != "" && !statusOK(r.Status) {
		return ""
	}
	for _, ps := range r.PropStats {
		if ps.Status != "" && !statusOK(ps.Status) {
			continue
		}
		if ps.Prop.CalendarData != nil {
			if data := strings.TrimSpace(ps.Prop.CalendarData.Data); data != "" {
				return data
			}
		}
	}
	return ""
}

// statusOK reports whether a "HTTP/1.1 200 OK"-style status line is a 2xx.
func statusOK(status string) bool {
	fields := strings.Fields(status)
	if len(fields) < 2 {
		return false
	}
	return strings.HasPrefix(fields[1], "2")
}
