package imap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/emersion/go-sasl"
	managesieve "github.com/hstern/go-managesieve"
)

// scriptedSASL is a sasl.Client that replays scripted responses and records the
// challenges it saw.
type scriptedSASL struct {
	mech       string
	ir         []byte
	responses  [][]byte
	err        error
	challenges [][]byte
}

func (s *scriptedSASL) Start() (string, []byte, error) { return s.mech, s.ir, s.err }
func (s *scriptedSASL) Next(ch []byte) ([]byte, error) {
	s.challenges = append(s.challenges, ch)
	if len(s.responses) == 0 {
		return nil, errors.New("unexpected challenge")
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

// TestSASLAdapterPassesThrough: the adapter must not alter what a mechanism
// returns: an initial response (nil, empty or non-empty), a response, or an error.
func TestSASLAdapterPassesThrough(t *testing.T) {
	for _, ir := range [][]byte{nil, {}, []byte("ir")} {
		_, got, err := newSASLAdapter(&scriptedSASL{mech: "X", ir: ir}).Start()
		if err != nil || (got == nil) != (ir == nil) || string(got) != string(ir) {
			t.Errorf("Start ir = %#v, %v; want %#v", got, err, ir)
		}
	}
	wantErr := errors.New("boom")
	if _, _, err := newSASLAdapter(&scriptedSASL{err: wantErr}).Start(); !errors.Is(err, wantErr) {
		t.Errorf("Start error = %v, want %v", err, wantErr)
	}
	a := newSASLAdapter(&scriptedSASL{responses: [][]byte{[]byte("x")}})
	if r, err := a.Next([]byte("c1")); err != nil || string(r) != "x" {
		t.Errorf("Next = %q, %v", r, err)
	}
	if _, err := a.Next(nil); err == nil {
		t.Error("error from the underlying client must propagate")
	}
}

// multiStepServer drives a three-leg exchange shaped like GSSAPI (RFC 4752): the
// client's initial response, a server challenge, a client response, then a final
// EMPTY client response before success.
type multiStepServer struct {
	mu    sync.Mutex
	steps int
	got   [][]byte
}

func (m *multiStepServer) Next(resp []byte) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, append([]byte(nil), resp...))
	m.steps++
	switch m.steps {
	case 1:
		return []byte("server-ap-rep"), false, nil
	case 2:
		return []byte("server-qop"), false, nil
	default:
		return nil, true, nil
	}
}

// authSession overrides memSession's authentication so tests can plug in any
// server-side SASL mechanism.
type authSession struct {
	*memSession
	mech   string
	server managesieve.SASLServer
}

func (s *authSession) AuthMechanisms() []string { return []string{s.mech} }
func (s *authSession) Authenticate(m string) (managesieve.SASLServer, error) {
	if m != s.mech {
		return nil, fmt.Errorf("unsupported mechanism %q", m)
	}
	return s.server, nil
}

type authBackend struct{ sess managesieve.Session }

func (b authBackend) NewSession(*managesieve.ServerConn) (managesieve.Session, error) {
	return b.sess, nil
}

func startAuthServer(t *testing.T, sess managesieve.Session) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := managesieve.NewServer(authBackend{sess})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}

// TestSASLAdapterMultiStepEmptyResponse runs a multi-step exchange with an empty
// final client response through a real go-managesieve connection.
func TestSASLAdapterMultiStepEmptyResponse(t *testing.T) {
	srv := &multiStepServer{}
	addr := startAuthServer(t, &authSession{memSession: &memSession{scripts: map[string]string{}}, mech: "GSSAPI", server: srv})
	cl := &Client{sieve: &manageSieveConfig{addr: addr, newSASL: func(context.Context) (sasl.Client, error) {
		return &scriptedSASL{mech: "GSSAPI", ir: []byte("ap-req"), responses: [][]byte{[]byte(""), []byte("qop-reply")}}, nil
	}}}
	if _, err := cl.ListRules(context.Background()); err != nil {
		t.Fatalf("ListRules over a multi-step SASL exchange: %v", err)
	}
	want := []string{"ap-req", "", "qop-reply"}
	if len(srv.got) != len(want) {
		t.Fatalf("server saw %d responses %q, want %d", len(srv.got), srv.got, len(want))
	}
	for i, w := range want {
		if string(srv.got[i]) != w {
			t.Errorf("response %d = %q, want %q", i, srv.got[i], w)
		}
	}
}

// TestResolveManageSieveMechanism: the SASL mechanism selection in the resolved
// config — PLAIN by default, OAUTHBEARER from a bearer token, or an explicit
// factory (GSSAPI), the latter two winning over PLAIN.
func TestResolveManageSieveMechanism(t *testing.T) {
	if resolveManageSieve(nil, "u", "p", "") != nil {
		t.Error("nil options must disable ManageSieve")
	}
	if c := resolveManageSieve(&ManageSieveOptions{Addr: "h:4190"}, "u", "p", ""); c == nil || c.newSASL != nil || c.username != "u" || c.password != "p" {
		t.Errorf("static config = %+v, want PLAIN with IMAP credentials", c)
	}

	c := resolveManageSieve(&ManageSieveOptions{Addr: "sieve.example.com:4190"}, "alice", "", "tok")
	if c == nil || c.newSASL == nil {
		t.Fatalf("bearer token must select a SASL factory, got %+v", c)
	}
	sc, err := c.newSASL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mech, ir, err := sc.Start()
	if err != nil || mech != "OAUTHBEARER" {
		t.Fatalf("Start = %q, %v", mech, err)
	}
	want := "n,a=alice,\x01host=sieve.example.com\x01port=4190\x01auth=Bearer tok\x01\x01"
	if string(ir) != want {
		t.Errorf("OAUTHBEARER initial response = %q, want %q", ir, want)
	}
	// Each call builds a fresh client.
	sc2, _ := c.newSASL(context.Background())
	if sc == sc2 {
		t.Error("newSASL must return a fresh client per connection")
	}

	called := false
	c = resolveManageSieve(&ManageSieveOptions{Addr: "h:4190", SASL: func(context.Context) (sasl.Client, error) {
		called = true
		return &scriptedSASL{mech: "GSSAPI"}, nil
	}}, "u", "", "tok")
	if _, err := c.newSASL(context.Background()); err != nil || !called {
		t.Errorf("explicit SASL factory must win over the bearer token (called=%v, err=%v)", called, err)
	}
}

// TestManageSieveOAuthBearerPerIdentity: a per-identity Client (bearer token set)
// authenticates ManageSieve with OAUTHBEARER against a real go-managesieve server
// and the CRUD path works over it.
func TestManageSieveOAuthBearerPerIdentity(t *testing.T) {
	var gotIR []byte
	sess := &authSession{memSession: &memSession{scripts: map[string]string{}}, mech: "OAUTHBEARER",
		server: oauthBearerServer(func(ir []byte) { gotIR = ir })}
	addr := startAuthServer(t, sess)
	cl := &Client{sieve: resolveManageSieve(&ManageSieveOptions{Addr: addr}, "alice", "", "tok-1")}
	ctx := context.Background()
	if _, err := cl.CreateRule(ctx, ruleFor("per identity")); err != nil {
		t.Fatalf("CreateRule via OAUTHBEARER: %v", err)
	}
	rules, err := cl.ListRules(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("ListRules = %v, %v", rules, err)
	}
	if len(gotIR) == 0 {
		t.Error("server never saw an OAUTHBEARER initial response")
	}
}

type oauthBearerServer func(ir []byte)

func (f oauthBearerServer) Next(resp []byte) ([]byte, bool, error) {
	if resp == nil {
		return []byte{}, false, nil
	}
	f(resp)
	want := "n,a=alice,\x01host=127.0.0.1\x01port="
	if len(resp) < len(want) || string(resp[:len(want)]) != want {
		return nil, false, errors.New("bad OAUTHBEARER GS2 header")
	}
	return nil, true, nil
}

// TestManageSieveSASLFactoryError: a factory failure (e.g. the credential
// exchange) surfaces as an error, not a silent PLAIN fallback.
func TestManageSieveSASLFactoryError(t *testing.T) {
	addr := startSieveServer(t, &memSession{scripts: map[string]string{}})
	boom := errors.New("exchange failed")
	cl := &Client{sieve: &manageSieveConfig{addr: addr, username: "tester", password: "secret",
		newSASL: func(context.Context) (sasl.Client, error) { return nil, boom }}}
	if _, err := cl.ListRules(context.Background()); !errors.Is(err, boom) {
		t.Errorf("ListRules error = %v, want it to wrap %v", err, boom)
	}
}
