package jmap

import (
	"context"
	"fmt"

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
