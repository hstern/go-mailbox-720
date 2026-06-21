package mailfilter

import (
	"context"
	"errors"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

// memStore is an in-memory ScriptStore holding a single Sieve-script string, so the
// CRUD and id logic is tested without any transport. The script text it stores is
// real Sieve produced by the translator, so the round-trip is exercised too.
type memStore struct {
	content string
	has     bool
}

func (s *memStore) ActiveScript(context.Context) (string, bool, error) { return s.content, s.has, nil }
func (s *memStore) SetActiveContent(_ context.Context, c string) error {
	s.content, s.has = c, true
	return nil
}

func rule(name string) mail.MessageRule {
	return mail.MessageRule{
		DisplayName: name, Enabled: true,
		Conditions: mail.RuleConditions{SubjectContains: []string{"x"}},
		Actions:    mail.RuleActions{Delete: true},
	}
}

func TestCRUD(t *testing.T) {
	s := &memStore{}
	ctx := context.Background()

	if rules, err := List(ctx, s); err != nil || len(rules) != 0 {
		t.Fatalf("empty List = %v, %v", rules, err)
	}

	a, err := Create(ctx, s, rule("a"))
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, _ := Create(ctx, s, rule("b"))
	if a.ID == "" || a.ID == b.ID {
		t.Fatalf("ids not unique: %q %q", a.ID, b.ID)
	}
	if a.Sequence != 1 || b.Sequence != 2 {
		t.Errorf("sequences = %d, %d; want 1, 2", a.Sequence, b.Sequence)
	}

	got, err := Get(ctx, s, a.ID)
	if err != nil || got.DisplayName != "a" {
		t.Fatalf("Get a = %+v, %v", got, err)
	}
	if _, err := Get(ctx, s, "nope"); !errors.Is(err, mail.ErrRuleNotFound) {
		t.Errorf("Get(missing) = %v, want ErrRuleNotFound", err)
	}

	upd := a
	upd.DisplayName = "a2"
	if _, err := Update(ctx, s, a.ID, upd); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ := Get(ctx, s, a.ID); got.DisplayName != "a2" {
		t.Errorf("after Update: %+v", got)
	}
	if _, err := Update(ctx, s, "ghost", upd); !errors.Is(err, mail.ErrRuleNotFound) {
		t.Errorf("Update(missing) = %v, want ErrRuleNotFound", err)
	}

	if err := Delete(ctx, s, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if rules, _ := List(ctx, s); len(rules) != 1 || rules[0].DisplayName != "b" {
		t.Errorf("after Delete: %+v", rules)
	}
	if err := Delete(ctx, s, a.ID); !errors.Is(err, mail.ErrRuleNotFound) {
		t.Errorf("Delete(missing) = %v, want ErrRuleNotFound", err)
	}
}

func TestFreshIDReusedAfterDelete(t *testing.T) {
	s := &memStore{}
	ctx := context.Background()
	a, _ := Create(ctx, s, rule("a")) // rule-1
	if err := Delete(ctx, s, a.ID); err != nil {
		t.Fatal(err)
	}
	c, _ := Create(ctx, s, rule("c")) // lowest unused -> rule-1 again
	if c.ID != "rule-1" {
		t.Errorf("fresh id after delete = %q, want rule-1", c.ID)
	}
}
