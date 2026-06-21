package imap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	managesieve "github.com/hstern/go-managesieve"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

// memSession is an in-memory ManageSieve Session (RFC 5804) shared across all
// connections to one test server, so the per-operation dial/logout the IMAP backend
// does still sees a consistent script store. It accepts SASL PLAIN for tester/secret.
type memSession struct {
	scripts map[string]string
	active  string
}

func (s *memSession) AuthMechanisms() []string { return []string{"PLAIN"} }

func (s *memSession) Authenticate(mech string) (managesieve.SASLServer, error) {
	if mech != "PLAIN" {
		return nil, fmt.Errorf("unsupported mechanism %q", mech)
	}
	return managesieve.PlainServer(func(_, user, pass string) error {
		if user != "tester" || pass != "secret" {
			return fmt.Errorf("bad credentials")
		}
		return nil
	}), nil
}

func (s *memSession) ListScripts() ([]managesieve.ScriptInfo, error) {
	out := make([]managesieve.ScriptInfo, 0, len(s.scripts))
	for name := range s.scripts {
		out = append(out, managesieve.ScriptInfo{Name: name, Active: name == s.active})
	}
	return out, nil
}

func (s *memSession) GetScript(name string) (string, error) {
	body, ok := s.scripts[name]
	if !ok {
		return "", fmt.Errorf("nonexistent script %q", name)
	}
	return body, nil
}

func (s *memSession) PutScript(name, body string) (string, error) {
	s.scripts[name] = body
	return "", nil
}

func (s *memSession) CheckScript(string) (string, error) { return "", nil }

func (s *memSession) SetActive(name string) error {
	s.active = name
	return nil
}

func (s *memSession) DeleteScript(name string) error {
	delete(s.scripts, name)
	if s.active == name {
		s.active = ""
	}
	return nil
}

func (s *memSession) RenameScript(oldName, newName string) error {
	body, ok := s.scripts[oldName]
	if !ok {
		return fmt.Errorf("nonexistent script %q", oldName)
	}
	s.scripts[newName] = body
	delete(s.scripts, oldName)
	if s.active == oldName {
		s.active = newName
	}
	return nil
}

func (s *memSession) HaveSpace(string, int64) error { return nil }
func (s *memSession) Logout() error                 { return nil }

type memBackend struct{ sess *memSession }

func (b *memBackend) NewSession(*managesieve.ServerConn) (managesieve.Session, error) {
	return b.sess, nil
}

// startSieveServer runs a go-managesieve server backed by sess on a loopback socket
// and returns its address; the IMAP backend connects to it per operation.
func startSieveServer(t *testing.T, sess *memSession) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := managesieve.NewServer(&memBackend{sess: sess})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}

// sieveClient builds an IMAP Client wired only with ManageSieve config (no IMAP
// connection — the filter methods use cl.sieve exclusively), pointed at addr.
func sieveClient(addr string) *Client {
	return &Client{sieve: &manageSieveConfig{addr: addr, username: "tester", password: "secret"}}
}

func ruleFor(name string) mail.MessageRule {
	return mail.MessageRule{
		DisplayName: name, Enabled: true,
		Conditions: mail.RuleConditions{SubjectContains: []string{"urgent"}},
		Actions:    mail.RuleActions{MoveToFolder: "Priority", MarkAsRead: true},
	}
}

func TestManageSieveFilterCRUD(t *testing.T) {
	addr := startSieveServer(t, &memSession{scripts: map[string]string{}})
	cl := sieveClient(addr)
	ctx := context.Background()

	if rules, err := cl.ListRules(ctx); err != nil || len(rules) != 0 {
		t.Fatalf("initial ListRules = %v, %v", rules, err)
	}

	created, err := cl.CreateRule(ctx, ruleFor("From boss"))
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created rule has no id")
	}

	// Reads back through a real Sieve round-trip over ManageSieve.
	rules, err := cl.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 1 || rules[0].DisplayName != "From boss" {
		t.Fatalf("rules = %+v", rules)
	}
	if rules[0].Actions.MoveToFolder != "Priority" || !rules[0].Actions.MarkAsRead {
		t.Errorf("actions lost in round-trip: %+v", rules[0].Actions)
	}

	got, err := cl.GetRule(ctx, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("GetRule = %+v, %v", got, err)
	}
	if _, err := cl.GetRule(ctx, "nope"); !errors.Is(err, mail.ErrRuleNotFound) {
		t.Errorf("GetRule(missing) = %v, want ErrRuleNotFound", err)
	}

	upd := got
	upd.DisplayName = "renamed"
	if _, err := cl.UpdateRule(ctx, created.ID, upd); err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	if after, _ := cl.GetRule(ctx, created.ID); after.DisplayName != "renamed" {
		t.Errorf("update not applied: %+v", after)
	}
	if _, err := cl.UpdateRule(ctx, "ghost", upd); !errors.Is(err, mail.ErrRuleNotFound) {
		t.Errorf("UpdateRule(missing) = %v, want ErrRuleNotFound", err)
	}

	if err := cl.DeleteRule(ctx, created.ID); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	if rules, _ := cl.ListRules(ctx); len(rules) != 0 {
		t.Errorf("after delete: %+v", rules)
	}
	if err := cl.DeleteRule(ctx, created.ID); !errors.Is(err, mail.ErrRuleNotFound) {
		t.Errorf("DeleteRule(missing) = %v, want ErrRuleNotFound", err)
	}
}

func TestManageSieveFiltersUnsupported(t *testing.T) {
	cl := &Client{} // no sieve config
	if _, err := cl.ListRules(context.Background()); !errors.Is(err, mail.ErrFiltersUnsupported) {
		t.Errorf("ListRules without ManageSieve = %v, want ErrFiltersUnsupported", err)
	}
	if _, err := cl.CreateRule(context.Background(), ruleFor("x")); !errors.Is(err, mail.ErrFiltersUnsupported) {
		t.Errorf("CreateRule without ManageSieve = %v, want ErrFiltersUnsupported", err)
	}
}
