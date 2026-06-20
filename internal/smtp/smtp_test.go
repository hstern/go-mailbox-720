package smtp

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// recordedMessage is one fully-received submission captured by the test backend.
type recordedMessage struct {
	from string
	to   []string
	data []byte
}

// recordingBackend is a minimal in-process go-smtp Backend that records every
// MAIL FROM, RCPT TO, and DATA it receives, optionally requiring auth.
type recordingBackend struct {
	mu       sync.Mutex
	messages []recordedMessage

	// requireUser/requirePass, when non-empty, make the session reject any
	// transaction that was not preceded by a matching AUTH.
	requireUser string
	requirePass string
}

func (b *recordingBackend) NewSession(_ *gosmtp.Conn) (gosmtp.Session, error) {
	return &recordingSession{be: b}, nil
}

func (b *recordingBackend) record(m recordedMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, m)
}

func (b *recordingBackend) recorded() []recordedMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recordedMessage(nil), b.messages...)
}

// recordingSession implements both Session and AuthSession so the server
// advertises AUTH and we can verify the credentials the client sent.
type recordingSession struct {
	be            *recordingBackend
	from          string
	to            []string
	authenticated bool
}

var (
	_ gosmtp.Session     = (*recordingSession)(nil)
	_ gosmtp.AuthSession = (*recordingSession)(nil)
)

// AuthMechanisms advertises PLAIN, which is the client's primary mechanism.
// (go-sasl ships only a PLAIN server implementation, so LOGIN cannot be
// exercised server-side here.)
func (s *recordingSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

// Auth returns a server-side SASL handler that validates the credentials
// against the backend's required user/pass.
func (s *recordingSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, &gosmtp.SMTPError{Code: 504, Message: "unsupported auth mechanism"}
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		return s.checkCreds(username, password)
	}), nil
}

func (s *recordingSession) checkCreds(username, password string) error {
	if username != s.be.requireUser || password != s.be.requirePass {
		return gosmtp.ErrAuthFailed
	}
	s.authenticated = true
	return nil
}

func (s *recordingSession) Mail(from string, _ *gosmtp.MailOptions) error {
	if s.be.requireUser != "" && !s.authenticated {
		return &gosmtp.SMTPError{Code: 530, Message: "authentication required"}
	}
	s.from = from
	return nil
}

func (s *recordingSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *recordingSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.be.record(recordedMessage{from: s.from, to: append([]string(nil), s.to...), data: data})
	return nil
}

func (s *recordingSession) Reset() { s.from, s.to = "", nil }

func (s *recordingSession) Logout() error { return nil }

// startServer stands up an in-process plaintext go-smtp server on a loopback
// port and returns its address. The server is shut down on test cleanup.
func startServer(t *testing.T, be gosmtp.Backend) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := gosmtp.NewServer(be)
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true // plaintext AUTH is fine for the in-process test
	srv.ReadTimeout = 5 * time.Second
	srv.WriteTimeout = 5 * time.Second

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Serve returns when the listener is closed; that is the normal stop path.
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return l.Addr().String()
}

const sampleMessage = "From: alice@example.com\r\n" +
	"To: bob@example.com\r\n" +
	"Subject: hello\r\n" +
	"\r\n" +
	"This is the body.\r\n"

func TestSend(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   []string
	}{
		{name: "single recipient", from: "alice@example.com", to: []string{"bob@example.com"}},
		{
			name: "multiple recipients",
			from: "alice@example.com",
			to:   []string{"bob@example.com", "carol@example.com", "dave@example.com"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := &recordingBackend{}
			addr := startServer(t, be)

			cl, err := Dial(addr, "", "", &Options{})
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = cl.Close() }()

			if err := cl.Send(context.Background(), tc.from, tc.to, []byte(sampleMessage)); err != nil {
				t.Fatalf("Send: %v", err)
			}

			got := be.recorded()
			if len(got) != 1 {
				t.Fatalf("recorded %d messages, want 1", len(got))
			}
			m := got[0]
			if m.from != tc.from {
				t.Errorf("MAIL FROM = %q, want %q", m.from, tc.from)
			}
			if !equalStrings(m.to, tc.to) {
				t.Errorf("RCPT TO = %v, want %v", m.to, tc.to)
			}
			if got, want := normalize(string(m.data)), normalize(sampleMessage); got != want {
				t.Errorf("DATA = %q, want %q", got, want)
			}
		})
	}
}

func TestSendWithAuth(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		pass    string
		wantErr bool
	}{
		{name: "correct credentials", user: "alice", pass: "s3cret"},
		{name: "wrong password", user: "alice", pass: "wrong", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := &recordingBackend{requireUser: "alice", requirePass: "s3cret"}
			addr := startServer(t, be)

			cl, err := Dial(addr, tc.user, tc.pass, &Options{})
			if tc.wantErr {
				if err == nil {
					_ = cl.Close()
					t.Fatal("Dial: want auth error, got nil")
				}
				if !strings.Contains(err.Error(), "smtp: auth:") {
					t.Errorf("Dial error = %v, want it to wrap %q", err, "smtp: auth:")
				}
				return
			}
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = cl.Close() }()

			from, to := "alice@example.com", []string{"bob@example.com"}
			if err := cl.Send(context.Background(), from, to, []byte(sampleMessage)); err != nil {
				t.Fatalf("Send: %v", err)
			}
			got := be.recorded()
			if len(got) != 1 || got[0].from != from {
				t.Fatalf("recorded %#v, want one message from %q", got, from)
			}
		})
	}
}

func TestSendAfterClose(t *testing.T) {
	be := &recordingBackend{}
	addr := startServer(t, be)

	cl, err := Dial(addr, "", "", &Options{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Sending on a closed client must fail rather than silently succeed.
	err = cl.Send(context.Background(), "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage))
	if err == nil {
		t.Fatal("Send after Close: want error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "smtp:") {
		t.Errorf("Send error = %v, want it to be wrapped with the smtp: prefix", err)
	}
}

func TestSendContextCancelled(t *testing.T) {
	be := &recordingBackend{}
	addr := startServer(t, be)

	cl, err := Dial(addr, "", "", &Options{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Send

	err = cl.Send(ctx, "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send error = %v, want it to wrap context.Canceled", err)
	}
	if got := be.recorded(); len(got) != 0 {
		t.Errorf("recorded %d messages on cancelled send, want 0", len(got))
	}
}

func TestDialError(t *testing.T) {
	// Nothing listening on this port: dialing must surface a wrapped error.
	_, err := Dial("127.0.0.1:1", "", "", &Options{})
	if err == nil {
		t.Fatal("Dial to dead port: want error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "smtp: dial") {
		t.Errorf("Dial error = %v, want it to start with %q", err, "smtp: dial")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// normalize trims a trailing newline so server-side and client-side line
// endings can be compared without fighting over the final CRLF.
func normalize(s string) string {
	return strings.TrimRight(s, "\r\n")
}
