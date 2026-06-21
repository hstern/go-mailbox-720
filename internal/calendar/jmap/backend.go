package jmap

import (
	"context"
	"fmt"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

func (cl *Client) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	args, err := cl.do(ctx, &calendarGet{Account: cl.accountID})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*calendarGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for Calendar/get", args)
	}
	out := make([]calendar.Calendar, 0, len(resp.List))
	for _, c := range resp.List {
		out = append(out, calendar.Calendar{ID: string(c.ID), Name: c.Name, Description: c.Description})
	}
	return out, nil
}

// ListEvents returns events in the given calendar, optionally bounded by r.
// It issues a CalendarEvent/query (masters only, no recurrence expansion) to
// obtain matching IDs, then fetches them with CalendarEvent/get via getEvents.
func (cl *Client) ListEvents(ctx context.Context, calendarID string, r calendar.Range) ([]calendar.Event, error) {
	filter := &eventFilter{
		InCalendars: []gojmap.ID{gojmap.ID(calendarID)},
	}
	if !r.Start.IsZero() {
		filter.After = r.Start.UTC().Format(time.RFC3339)
	}
	if !r.End.IsZero() {
		filter.Before = r.End.UTC().Format(time.RFC3339)
	}

	qArgs, err := cl.do(ctx, &eventQuery{
		Account: cl.accountID,
		Filter:  filter,
		// ExpandRecurrences is false: return masters only.
	})
	if err != nil {
		return nil, fmt.Errorf("jmap: CalendarEvent/query: %w", err)
	}
	qResp, ok := qArgs.(*eventQueryResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for CalendarEvent/query", qArgs)
	}
	if len(qResp.IDs) == 0 {
		return nil, nil
	}
	return cl.getEvents(ctx, qResp.IDs)
}

// getEvents fetches the CalendarEvents identified by ids and maps each one to a
// calendar.Event via toCalendarEvent. It is reused by GetEvent, ListInstances,
// and delta-sync (Tasks 7, 9, 12). The Properties list requests all fields that
// toCalendarEvent needs, including the UTC-normalised instants utcStart/utcEnd.
func (cl *Client) getEvents(ctx context.Context, ids []gojmap.ID) ([]calendar.Event, error) {
	gArgs, err := cl.do(ctx, &eventGet{
		Account: cl.accountID,
		IDs:     ids,
		Properties: []string{
			"id", "uid", "calendarIds", "baseEventId",
			"title", "description", "descriptionContentType",
			"created", "start", "timeZone", "duration",
			"utcStart", "utcEnd",
			"showWithoutTime", "status", "sequence",
			"locations", "participants",
			"recurrenceRules", "recurrenceOverrides",
			"recurrenceId", "recurrenceIdTimeZone",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("jmap: CalendarEvent/get: %w", err)
	}
	gResp, ok := gArgs.(*eventGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for CalendarEvent/get", gArgs)
	}
	out := make([]calendar.Event, 0, len(gResp.List))
	for _, ce := range gResp.List {
		ev, err := toCalendarEvent(ce)
		if err != nil {
			return nil, fmt.Errorf("jmap: toCalendarEvent %s: %w", ce.ID, err)
		}
		out = append(out, ev)
	}
	return out, nil
}
