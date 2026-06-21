package jmap

import (
	"context"
	"fmt"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// ListInstances expands the recurring series identified by eventID into its
// individual occurrences within the bounded time window r. A bounded range is
// required on both sides: recurrence expansion needs both before and after per
// the JMAP Calendar draft, and an RRULE without COUNT/UNTIL is infinite.
//
// The method issues a CalendarEvent/query with expandRecurrences:true and the
// given time bounds, fetches the synthetic instance objects with getEvents
// (which maps baseEventId → SeriesMasterID and recurrenceId → RecurrenceID via
// toCalendarEvent), then keeps only the instances whose SeriesMasterID matches
// the requested eventID. The query is account-wide; the filtering step isolates
// instances of the requested series.
func (cl *Client) ListInstances(ctx context.Context, eventID string, r calendar.Range) ([]calendar.Event, error) {
	if r.Start.IsZero() || r.End.IsZero() {
		return nil, fmt.Errorf("jmap: ListInstances requires a bounded Range (both Start and End must be non-zero)")
	}

	qArgs, err := cl.do(ctx, &eventQuery{
		Account: cl.accountID,
		Filter: &eventFilter{
			After:  r.Start.UTC().Format(time.RFC3339),
			Before: r.End.UTC().Format(time.RFC3339),
		},
		ExpandRecurrences: true,
	})
	if err != nil {
		return nil, fmt.Errorf("jmap: CalendarEvent/query (expandRecurrences): %w", err)
	}
	qResp, ok := qArgs.(*eventQueryResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for CalendarEvent/query", qArgs)
	}
	if len(qResp.IDs) == 0 {
		return nil, nil
	}

	all, err := cl.getEvents(ctx, qResp.IDs)
	if err != nil {
		return nil, err
	}

	// Filter to instances that belong to the requested series.
	out := make([]calendar.Event, 0, len(all))
	for _, ev := range all {
		if ev.SeriesMasterID == eventID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Assert that Client implements the calendar.InstanceReader interface.
var _ calendar.InstanceReader = (*Client)(nil)
