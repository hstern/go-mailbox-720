package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// Verify that Client implements the DeltaReader interface.
var _ calendar.DeltaReader = (*Client)(nil)

// Delta returns the events in calendarID that have changed since the opaque
// token (a JMAP state string). It issues a CalendarEvent/changes call and then
// fetches the created+updated events via getEvents.
//
// Calendar filtering for changed events: CalendarEvent/changes is account-wide,
// not scoped to a single calendar. We therefore fetch all created/updated
// events and retain only those whose CalendarID matches the requested
// calendarID. Events in other calendars are silently dropped.
//
// Calendar filtering for destroyed events: destroyed IDs cannot be fetched
// (the events no longer exist on the server), so we cannot determine which
// calendar they belonged to. All destroyed IDs are included in removed
// regardless of calendar. Callers should treat removed as a superset and
// handle tombstones idempotently.
//
// Limitation: a single call returns one page of changes. If the server sets
// hasMoreChanges, additional changes exist beyond the returned next token; the
// caller must call Delta again with next until the server stops advancing.
// This method does not loop internally.
func (cl *Client) Delta(ctx context.Context, calendarID string, token string) (changed []calendar.Event, removed []string, next string, err error) {
	cArgs, err := cl.do(ctx, &eventChanges{
		Account:    cl.accountID,
		SinceState: token,
	})
	if err != nil {
		return nil, nil, "", fmt.Errorf("jmap: CalendarEvent/changes: %w", err)
	}
	cResp, ok := cArgs.(*eventChangesResponse)
	if !ok {
		return nil, nil, "", fmt.Errorf("jmap: unexpected response %T for CalendarEvent/changes", cArgs)
	}

	next = cResp.NewState

	// Map Destroyed IDs to removed strings (all included; see doc comment above).
	if len(cResp.Destroyed) > 0 {
		removed = make([]string, len(cResp.Destroyed))
		for i, id := range cResp.Destroyed {
			removed[i] = string(id)
		}
	}

	// Collect created + updated IDs and fetch them.
	toFetch := make([]gojmap.ID, 0, len(cResp.Created)+len(cResp.Updated))
	toFetch = append(toFetch, cResp.Created...)
	toFetch = append(toFetch, cResp.Updated...)

	if len(toFetch) == 0 {
		return nil, removed, next, nil
	}

	events, err := cl.getEvents(ctx, toFetch)
	if err != nil {
		return nil, nil, "", err
	}

	// Filter to the requested calendar only.
	for _, ev := range events {
		if ev.CalendarID == calendarID {
			changed = append(changed, ev)
		}
	}

	return changed, removed, next, nil
}
