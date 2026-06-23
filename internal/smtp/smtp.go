// Package smtp implements the outbound SMTP transport: a backend-neutral
// message-submission port that sends a pre-built RFC 822 message to a set of
// recipients over an SMTP server, using emersion/go-smtp for the protocol and
// emersion/go-sasl for authentication. A Client is bound to one connection to
// the operator's submission server.
//
// This is the transport only — given a fully composed RFC 822 message and an
// envelope (MAIL FROM / RCPT TO), it submits it. Composing the message bytes
// (for example the iTIP/iMIP scheduling mails that will use this port to send
// REPLY/REQUEST/CANCEL) is a separate concern handled by the caller.
//
// Connection security follows Options: implicit TLS (smtps, port 465),
// STARTTLS upgrade from plaintext (submission, port 587), or plaintext (local
// servers and tests only). Authentication follows Options too: a bearer token
// selects SASL OAUTHBEARER (RFC 7628, the per-identity path); otherwise, when a
// username is supplied, Dial authenticates with SASL PLAIN, falling back to LOGIN
// when the server advertises only LOGIN.
package smtp

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// Sender is the outbound message-submission port: submit a pre-built RFC 822
// message and release the connection. The iTIP/iMIP scheduling engine (and any
// other send path) depends on this neutral interface rather than the concrete
// SMTP Client, so an alternative transport can drop in behind it later.
type Sender interface {
	// Send submits raw (an RFC 822 message) from the from envelope address to
	// every address in to.
	Send(ctx context.Context, from string, to []string, raw []byte) error
	// Close releases the underlying connection.
	Close() error
}

// Options configures the SMTP connection's transport security. TLS and StartTLS
// are mutually exclusive; if both are set, TLS (implicit) takes precedence.
type Options struct {
	// TLS dials with implicit TLS (smtps, typically port 465): the connection
	// is encrypted from the first byte.
	TLS bool
	// StartTLS dials plaintext and then issues STARTTLS to upgrade to TLS
	// (submission, typically port 587).
	StartTLS bool
	// BearerToken, when non-empty, authenticates with SASL OAUTHBEARER (RFC 7628)
	// using this token instead of SASL PLAIN/LOGIN — the per-identity path
	// (MB720-43), where the token is an exchanged backend-audience access token. The
	// Dial username becomes the OAUTHBEARER authorization identity and the Dial
	// password is ignored.
	BearerToken string
}

// Client is an SMTP-backed Sender over a single connection to a submission
// server.
type Client struct {
	c *gosmtp.Client
}

var _ Sender = (*Client)(nil)

// Dial connects to addr (host:port) per o, and — when username is non-empty —
// authenticates with SASL (PLAIN, or LOGIN when the server advertises only
// LOGIN). A nil o defaults to implicit TLS. It returns a Client ready to Send.
func Dial(addr, username, password string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{TLS: true}
	}
	var (
		c   *gosmtp.Client
		err error
	)
	switch {
	case o.TLS:
		c, err = gosmtp.DialTLS(addr, nil)
	case o.StartTLS:
		c, err = gosmtp.DialStartTLS(addr, nil)
	default:
		c, err = gosmtp.Dial(addr)
	}
	if err != nil {
		return nil, fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	if o.BearerToken != "" {
		if err := authenticateBearer(c, addr, username, o.BearerToken); err != nil {
			_ = c.Close()
			return nil, err
		}
	} else if username != "" {
		if err := authenticate(c, username, password); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return &Client{c: c}, nil
}

// authenticate logs in with PLAIN, falling back to LOGIN when the server
// advertises LOGIN but not PLAIN.
func authenticate(c *gosmtp.Client, username, password string) error {
	auth := sasl.Client(sasl.NewPlainClient("", username, password))
	if !c.SupportsAuth(sasl.Plain) && c.SupportsAuth(sasl.Login) {
		auth = sasl.NewLoginClient(username, password)
	}
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("smtp: auth: %w", err)
	}
	return nil
}

// authenticateBearer logs in with SASL OAUTHBEARER (RFC 7628): username is the
// authorization identity, token the exchanged backend-audience access token.
func authenticateBearer(c *gosmtp.Client, addr, username, token string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		host, portStr = addr, ""
	}
	port, _ := strconv.Atoi(portStr)
	auth := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: username, Token: token, Host: host, Port: port,
	})
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("smtp: oauthbearer: %w", err)
	}
	return nil
}

// Send submits raw (a complete RFC 822 message, CRLF-terminated lines, headers
// then a blank line then body) from the envelope sender from to every recipient
// in to via MAIL FROM / RCPT TO / DATA. Bcc recipients are passed in to but
// must be absent from raw's headers, per SMTP submission convention.
//
// ctx cancellation is honored at the start and before issuing DATA — go-smtp's
// blocking command calls are not themselves ctx-aware, so once a command is in
// flight it runs to completion (or its timeout); the transaction is reset on
// any failure so the connection can be reused.
func (cl *Client) Send(ctx context.Context, from string, to []string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	if err := cl.c.Mail(from, nil); err != nil {
		_ = cl.c.Reset()
		return fmt.Errorf("smtp: mail from %q: %w", from, err)
	}
	for _, rcpt := range to {
		if err := cl.c.Rcpt(rcpt, nil); err != nil {
			_ = cl.c.Reset()
			return fmt.Errorf("smtp: rcpt to %q: %w", rcpt, err)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = cl.c.Reset()
		return fmt.Errorf("smtp: send: %w", err)
	}
	w, err := cl.c.Data()
	if err != nil {
		_ = cl.c.Reset()
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp: write: %w", err)
	}
	// Close commits the DATA transaction (sends the final dot and reads the
	// server's reply); its error is the delivery result.
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: data close: %w", err)
	}
	return nil
}

// Ping issues an SMTP NOOP to confirm the connection is still alive. The
// connection pool calls it on checkout so a connection the server has dropped
// while idle is discarded and re-dialed rather than handed out dead (MB720-53).
func (cl *Client) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cl.c.Noop(); err != nil {
		return fmt.Errorf("smtp: noop: %w", err)
	}
	return nil
}

// Close issues QUIT and closes the connection. A failed QUIT still tears down
// the socket.
func (cl *Client) Close() error {
	if err := cl.c.Quit(); err != nil {
		_ = cl.c.Close()
		return fmt.Errorf("smtp: quit: %w", err)
	}
	return nil
}
