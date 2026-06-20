package caldav

import (
	"context"
	"fmt"

	"github.com/emersion/go-ical"
	gocaldav "github.com/emersion/go-webdav/caldav"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

var _ calendar.DeltaReader = (*Client)(nil)

// eventCompRequest asks for each VEVENT's full set of properties — the same shape
// the read path uses, so delta objects map like ListEvents' do.
func eventCompRequest() gocaldav.CalendarCompRequest {
	return gocaldav.CalendarCompRequest{
		Name:     ical.CompCalendar,
		AllProps: true,
		Comps:    []gocaldav.CalendarCompRequest{{Name: ical.CompEvent, AllProps: true}},
	}
}

// Delta performs an RFC 6578 sync-collection against the calendar collection and
// maps the changed objects to neutral Events. An empty token is the initial sync
// (every current object plus a fresh sync-token); a non-empty token returns only
// the objects created or modified since, plus the next token to feed back.
//
// go-webdav's caldav.Client.SyncCollection reports the changed hrefs (path/ETag,
// not the bodies), so a follow-up MultiGetCalendar fetches the iCalendar data for
// the changed objects, which then map through the same eventFromObject/mapEvent
// path as ListEvents — each Event stamped with the opaque event id encoding its
// href.
//
// Removed resources (SyncResponse.Deleted) are returned as the opaque IDs the
// caller can emit as Graph @removed tombstones.
func (cl *Client) Delta(ctx context.Context, calID string, token string) (changed []calendar.Event, removed []string, next string, err error) {
	calPath, err := decodeCalendarID(calID)
	if err != nil {
		return nil, nil, "", err
	}

	sync, err := cl.c.SyncCollection(ctx, calPath, &gocaldav.SyncQuery{
		CompRequest: eventCompRequest(),
		SyncToken:   token,
	})
	if err != nil {
		return nil, nil, "", fmt.Errorf("caldav: delta: %w", err)
	}

	for _, href := range sync.Deleted {
		removed = append(removed, eventID(href))
	}
	if len(sync.Updated) == 0 {
		return nil, removed, sync.SyncToken, nil
	}

	paths := make([]string, 0, len(sync.Updated))
	for _, o := range sync.Updated {
		paths = append(paths, o.Path)
	}
	objs, err := cl.c.MultiGetCalendar(ctx, calPath, &gocaldav.CalendarMultiGet{
		Paths:       paths,
		CompRequest: eventCompRequest(),
	})
	if err != nil {
		return nil, nil, "", fmt.Errorf("caldav: delta fetch: %w", err)
	}

	changed = make([]calendar.Event, 0, len(objs))
	for _, o := range objs {
		if o.Data == nil {
			continue
		}
		if e, ok := eventFromObject(calID, o.Path, o.Data); ok {
			changed = append(changed, e)
		}
	}
	return changed, removed, sync.SyncToken, nil
}
