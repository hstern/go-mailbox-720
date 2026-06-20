package e2e

// smtpSink is an in-process SMTP submission server that captures the messages
// mailboxd sends. It is the outbound half of the iTIP/iMIP cooperation proving
// ground: when a Graph client POSTs an event with an attendee, mailboxd (with
// -enable-scheduling and an -smtp-addr pointing here) emails a METHOD:REQUEST
// iMIP invitation, and the sink records it for the test to assert.
//
// It is a real SMTP server (emersion/go-smtp) rather than a container because
// the transport under test is mailboxd's SMTP client; an in-process listener is
// enough to exercise it and keeps the fixture self-contained (no mail-relay
// container to provision). It accepts plaintext AUTH-less submission on a
// loopback port, the same posture mailboxd uses with -smtp-tls=false.

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"
)

// capturedMail is one submitted message: its envelope plus the raw RFC 822 body.
type capturedMail struct {
	from string
	to   []string
	data []byte
}

// smtpSink collects every message submitted to it.
type smtpSink struct {
	addr string

	mu   sync.Mutex
	mail []capturedMail
}

// messages returns a snapshot of the captured messages.
func (s *smtpSink) messages() []capturedMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedMail, len(s.mail))
	copy(out, s.mail)
	return out
}

// add records one captured message.
func (s *smtpSink) add(m capturedMail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mail = append(s.mail, m)
}

// startSMTPSink starts an in-process SMTP server on a free loopback port and
// returns it. The server is stopped via t.Cleanup.
func startSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	addr := freeAddr(t)
	sink := &smtpSink{addr: addr}

	srv := gosmtp.NewServer(&sinkBackend{sink: sink})
	srv.Addr = addr
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true
	srv.ReadTimeout = 10 * time.Second
	srv.WriteTimeout = 10 * time.Second

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("smtp sink listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return sink
}

// sinkBackend implements the go-smtp Backend interface, handing every session a
// session that appends its message to the sink.
type sinkBackend struct{ sink *smtpSink }

func (b *sinkBackend) NewSession(_ *gosmtp.Conn) (gosmtp.Session, error) {
	return &sinkSession{sink: b.sink}, nil
}

// sinkSession accumulates one message's envelope and data.
type sinkSession struct {
	sink *smtpSink
	from string
	to   []string
}

func (s *sinkSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *sinkSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *sinkSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.sink.add(capturedMail{from: s.from, to: s.to, data: data})
	return nil
}

func (s *sinkSession) Reset() { s.from = ""; s.to = nil }

func (s *sinkSession) Logout() error { return nil }
