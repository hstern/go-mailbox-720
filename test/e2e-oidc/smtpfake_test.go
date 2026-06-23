package e2e

// smtpfake_test.go is an in-process SMTP submission server that authenticates
// each connection via SASL OAUTHBEARER (RFC 7628). It is the outbound
// (submission) end of the MB720-52 impersonation chain: mailboxd, having
// RFC 8693-exchanged the user's token for the SMTP backend audience, dials this
// server with that exchanged token as its OAUTHBEARER bearer. The fake
// introspects the bearer via the same RFC 7662 validator a real backend would
// (tokenValidator), resolves the subject, and records every message it delivers
// under that subject in the shared userStore — so an iMIP REPLY mailboxd sends
// while acting as userA is recorded under userA's subject and never userB's.
//
// Why a hand-rolled server rather than emersion/go-smtp's (as smtpsink_test.go
// uses): the per-identity SMTP path sets the envelope sender (MAIL FROM) to the
// authenticated principal's bare subject claim, which for Zitadel's
// client-credentials users is a numeric id with no "@". go-smtp's server enforces
// RFC 5321 reverse-path syntax (local@domain) and rejects such a sender at the
// protocol layer before the backend ever sees it. A real deployment maps
// -auth-subject-claim to the user's email so the sender is a valid address; this
// e2e cannot, so the fake speaks just enough SMTP to accept any MAIL FROM while
// still performing a real OAUTHBEARER exchange (via sasl.NewOAuthBearerServer)
// and recording under the introspected subject. The bearer token is never logged.

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
)

// startSMTPFake starts an in-process SMTP server on a free loopback port and
// returns its host:port. Each session authenticates via SASL OAUTHBEARER,
// validating the bearer with v (and requiring the OAUTHBEARER authzid, when
// present, to equal the resulting subject) and recording every delivered message
// under that subject via store.recordSent. The listener is closed via t.Cleanup.
func startSMTPFake(t *testing.T, v *tokenValidator, store *userStore) (addr string) {
	t.Helper()
	addr = freeAddr(t)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("smtp fake listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by t.Cleanup
			}
			go (&smtpFakeConn{v: v, store: store, conn: conn}).serve()
		}
	}()

	return addr
}

// smtpFakeConn is one connection: it carries the validator and store, the subject
// resolved by a successful OAUTHBEARER auth, and the envelope of the message
// currently being delivered.
type smtpFakeConn struct {
	v     *tokenValidator
	store *userStore
	conn  net.Conn

	sub  string // resolved subject after a successful OAUTHBEARER auth
	from string
	to   []string
}

// serve speaks the minimal SMTP submission dialogue mailboxd's client drives:
// EHLO (advertising AUTH OAUTHBEARER), AUTH OAUTHBEARER, MAIL FROM, RCPT TO,
// DATA, RSET, and QUIT. Unknown verbs get a benign 250 so a stray command never
// wedges the exchange. Any I/O error ends the connection.
func (c *smtpFakeConn) serve() {
	defer func() { _ = c.conn.Close() }()
	r := bufio.NewReader(c.conn)

	if !c.reply("220 smtp fake ready") {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg := splitVerb(line)
		switch verb {
		case "EHLO", "HELO":
			// Advertise AUTH OAUTHBEARER so the client picks that mechanism.
			if !c.reply("250-smtp fake\r\n250 AUTH OAUTHBEARER") {
				return
			}
		case "AUTH":
			if !c.handleAuth(r, arg) {
				return
			}
		case "MAIL":
			// Accept any reverse-path, including a bare (non-email) subject; the
			// recorded identity comes from the OAUTHBEARER subject, not this address.
			c.from = reversePath(arg)
			if !c.reply("250 OK") {
				return
			}
		case "RCPT":
			c.to = append(c.to, reversePath(arg))
			if !c.reply("250 OK") {
				return
			}
		case "DATA":
			if !c.handleData(r) {
				return
			}
		case "RSET":
			c.from, c.to = "", nil
			if !c.reply("250 OK") {
				return
			}
		case "QUIT":
			c.reply("221 Bye")
			return
		case "NOOP":
			if !c.reply("250 OK") {
				return
			}
		default:
			if !c.reply("250 OK") {
				return
			}
		}
	}
}

// handleAuth completes a single-round SASL OAUTHBEARER exchange. mailboxd's
// client sends the initial response inline ("AUTH OAUTHBEARER <base64>"), so one
// Next call decides the outcome: success yields 235; any failure yields 535 and
// the token is never logged.
func (c *smtpFakeConn) handleAuth(_ *bufio.Reader, arg string) bool {
	mech, ir := splitVerb(arg)
	if !strings.EqualFold(mech, sasl.OAuthBearer) {
		return c.reply("504 unsupported authentication mechanism")
	}
	resp, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ir))
	if err != nil {
		return c.reply("535 authentication failed")
	}

	srv := sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		sub, err := c.v.validate(opts.Token)
		if err != nil {
			// Never log opts.Token.
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		}
		if opts.Username != "" && opts.Username != sub {
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		}
		c.sub = sub
		return nil
	})

	_, done, err := srv.Next(resp)
	if err != nil || !done || c.sub == "" {
		c.sub = ""
		return c.reply("535 authentication failed")
	}
	return c.reply("235 authentication succeeded")
}

// handleData reads the message body up to the SMTP "<CRLF>.<CRLF>" terminator and
// records it under the authenticated subject. A DATA before a successful AUTH is
// rejected, so a recorded message always binds to a validated identity.
func (c *smtpFakeConn) handleData(r *bufio.Reader) bool {
	if c.sub == "" {
		return c.reply("530 authentication required")
	}
	if !c.reply("354 End data with <CR><LF>.<CR><LF>") {
		return false
	}

	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return false
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			break
		}
		// Undo dot-stuffing (RFC 5321 4.5.2): a leading "." was doubled by the client.
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}
		b.WriteString(trimmed)
		b.WriteString("\r\n")
	}

	c.store.recordSent(c.sub, sentMail{From: c.from, To: c.to, Data: b.String()})
	c.from, c.to = "", nil
	return c.reply("250 OK: message accepted")
}

// reply writes one CRLF-terminated SMTP response line, reporting whether the
// write succeeded (a failed write ends the connection).
func (c *smtpFakeConn) reply(s string) bool {
	_, err := fmt.Fprintf(c.conn, "%s\r\n", s)
	return err == nil
}

// splitVerb splits a command line into its first token (upper-cased) and the
// remainder. It is used both for the SMTP verb and for the AUTH mechanism/IR.
func splitVerb(line string) (verb, rest string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return strings.ToUpper(line[:i]), strings.TrimSpace(line[i+1:])
	}
	return strings.ToUpper(line), ""
}

// reversePath extracts the address from a "FROM:<addr>" / "TO:<addr>" argument,
// stripping the FROM:/TO: prefix and any angle brackets. Trailing ESMTP
// parameters (e.g. " SIZE=...") are dropped. It does not validate the address —
// the fake deliberately accepts bare, non-email reverse paths.
func reversePath(arg string) string {
	if i := strings.IndexByte(arg, ':'); i >= 0 {
		arg = arg[i+1:]
	}
	arg = strings.TrimSpace(arg)
	if i := strings.IndexByte(arg, ' '); i >= 0 {
		arg = arg[:i]
	}
	arg = strings.TrimPrefix(arg, "<")
	arg = strings.TrimSuffix(arg, ">")
	return arg
}
