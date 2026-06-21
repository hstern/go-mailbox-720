// Package jmap implements the internal/calendar port over JMAP for Calendars
// (draft-ietf-jmap-calendars, capability urn:ietf:params:jmap:calendars). It is
// the JMAP-native counterpart of the CalDAV adapter; CalendarEvent objects are
// JSCalendar (RFC 8984) events handled via github.com/hstern/go-jscalendar.
package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

const calendarsURI gojmap.URI = "urn:ietf:params:jmap:calendars"

// Options configures the JMAP calendars connection.
type Options struct {
	// SessionEndpoint overrides the JMAP Session resource URL. When empty, Dial
	// uses the URL passed to Dial as the session endpoint.
	SessionEndpoint string
}

// Client is a JMAP-backed calendar.Backend over one authenticated session and
// calendars account.
type Client struct {
	c         *gojmap.Client
	accountID gojmap.ID
}

// Dial authenticates to the JMAP server, fetches the Session, and resolves the
// primary calendars account. The access token is the operator's JMAP credential,
// always sourced from an environment secret at the call site.
func Dial(sessionURL, accessToken string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{}
	}
	endpoint := o.SessionEndpoint
	if endpoint == "" {
		endpoint = sessionURL
	}
	c := &gojmap.Client{SessionEndpoint: endpoint}
	c.WithAccessToken(accessToken)
	if err := c.Authenticate(); err != nil {
		return nil, fmt.Errorf("jmap: authenticate: %w", err)
	}
	accountID, ok := c.Session.PrimaryAccounts[calendarsURI]
	if !ok || accountID == "" {
		return nil, fmt.Errorf("jmap: session advertises no primary calendars account (%s)", calendarsURI)
	}
	return &Client{c: c, accountID: accountID}, nil
}

// newClient wraps an already-configured go-jmap client and account id — the seam
// tests use to inject a client pointed at an httptest server.
func newClient(c *gojmap.Client, accountID gojmap.ID) *Client {
	return &Client{c: c, accountID: accountID}
}

// Close releases the backend. The JMAP client is stateless over HTTP, so there is
// nothing to close.
func (cl *Client) Close() error { return nil }

// do issues a one-call JMAP request and returns the single response argument,
// surfacing a server MethodError as a Go error.
func (cl *Client) do(ctx context.Context, m gojmap.Method) (any, error) {
	req := &gojmap.Request{Context: ctx}
	req.Invoke(m)
	resp, err := cl.c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jmap: request: %w", err)
	}
	if len(resp.Responses) == 0 {
		return nil, fmt.Errorf("jmap: empty response")
	}
	args := resp.Responses[0].Args
	if me, ok := args.(*gojmap.MethodError); ok {
		return nil, fmt.Errorf("jmap: method error: %w", me)
	}
	return args, nil
}
