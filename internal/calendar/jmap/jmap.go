// Package jmap implements the internal/calendar port over JMAP for Calendars
// (draft-ietf-jmap-calendars, capability urn:ietf:params:jmap:calendars). It is
// the JMAP-native counterpart of the CalDAV adapter; CalendarEvent objects are
// JSCalendar (RFC 8984) events handled via github.com/hstern/go-jscalendar.
package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

const calendarsURI gojmap.URI = "urn:ietf:params:jmap:calendars"

// Options configures the JMAP calendars connection.
type Options struct {
	// SessionEndpoint overrides the JMAP Session resource URL. When empty, Dial
	// uses the URL passed to Dial as the session endpoint.
	SessionEndpoint string

	// BasicAuth, when non-nil, authenticates via HTTP Basic Auth instead of the
	// Bearer token passed to Dial. Stalwart and some other servers use HTTP Basic
	// Auth for their user-facing JMAP endpoint rather than OAuth 2.0 bearer
	// tokens; setting this causes Dial to call WithBasicAuth on the go-jmap
	// client, overriding the accessToken argument.
	BasicAuth *BasicAuthCredentials

	// APIURLOverride, when non-empty, replaces the apiUrl value that the server
	// returns in its Session resource. This is useful when the server advertises
	// an internal hostname (e.g. a Docker container's short hostname) that is
	// not reachable from the caller's network; set APIURLOverride to the
	// externally-reachable JMAP API endpoint instead.
	APIURLOverride string
}

// BasicAuthCredentials holds username and password for HTTP Basic authentication.
type BasicAuthCredentials struct {
	Username string
	Password string
}

// Client is a JMAP-backed calendar.Backend over one authenticated session and
// calendars account.
type Client struct {
	c         *gojmap.Client
	accountID gojmap.ID
	// token is the bearer access token that authenticated the session, retained
	// so the RFC 8887 WebSocket watch (watch.go) can authenticate its own socket.
	// It is empty when the session uses HTTP Basic auth, in which case watching
	// is unavailable (the push consumer is bearer-only).
	token string
}

// Dial authenticates to the JMAP server, fetches the Session, and resolves the
// primary calendars account. The access token is the operator's JMAP credential,
// always sourced from an environment secret at the call site. When
// opts.BasicAuth is set the token is ignored and HTTP Basic Auth is used
// instead — required for servers (e.g. Stalwart) that authenticate their JMAP
// endpoint with username:password rather than bearer tokens.
func Dial(sessionURL, accessToken string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{}
	}
	endpoint := o.SessionEndpoint
	if endpoint == "" {
		endpoint = sessionURL
	}
	c := &gojmap.Client{SessionEndpoint: endpoint}
	if o.BasicAuth != nil {
		c.WithBasicAuth(o.BasicAuth.Username, o.BasicAuth.Password)
	} else {
		c.WithAccessToken(accessToken)
	}
	if err := c.Authenticate(); err != nil {
		return nil, fmt.Errorf("jmap: authenticate: %w", err)
	}
	// Apply the API URL override AFTER authentication so that all subsequent
	// method calls (which go to Session.APIURL) reach the externally-visible
	// endpoint rather than any internal hostname the server may have advertised.
	if o.APIURLOverride != "" {
		c.Session.APIURL = o.APIURLOverride
	}
	accountID, ok := c.Session.PrimaryAccounts[calendarsURI]
	if !ok || accountID == "" {
		return nil, fmt.Errorf("jmap: session advertises no primary calendars account (%s)", calendarsURI)
	}
	// The bearer token is only retained for the WebSocket watch; under Basic auth
	// there is no bearer to reuse, so token stays empty and Watch reports that
	// push is unavailable.
	var token string
	if o.BasicAuth == nil {
		token = accessToken
	}
	return &Client{c: c, accountID: accountID, token: token}, nil
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

// Verify that Client implements the Backend interface.
var _ calendar.Backend = (*Client)(nil)
