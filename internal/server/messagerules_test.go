package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// filterBackend is a mail.Backend implementing the FilterReader and FilterWriter
// capabilities over an in-memory, order-preserving rule set — the fake the
// messageRules handlers drive in these tests. It embeds bareMailBackend (defined
// in quota_test.go) for the required Backend methods.
type filterBackend struct {
	bareMailBackend
	rules  []mail.MessageRule
	nextID int
}

func (b *filterBackend) ListRules(context.Context) ([]mail.MessageRule, error) {
	out := make([]mail.MessageRule, len(b.rules))
	copy(out, b.rules)
	return out, nil
}

func (b *filterBackend) GetRule(_ context.Context, id string) (mail.MessageRule, error) {
	for _, r := range b.rules {
		if r.ID == id {
			return r, nil
		}
	}
	return mail.MessageRule{}, mail.ErrRuleNotFound
}

func (b *filterBackend) CreateRule(_ context.Context, rule mail.MessageRule) (mail.MessageRule, error) {
	b.nextID++
	rule.ID = fmt.Sprintf("rule-%d", b.nextID)
	b.rules = append(b.rules, rule)
	return rule, nil
}

func (b *filterBackend) UpdateRule(_ context.Context, id string, rule mail.MessageRule) (mail.MessageRule, error) {
	for i, r := range b.rules {
		if r.ID == id {
			rule.ID = id
			b.rules[i] = rule
			return rule, nil
		}
	}
	return mail.MessageRule{}, mail.ErrRuleNotFound
}

func (b *filterBackend) DeleteRule(_ context.Context, id string) error {
	for i, r := range b.rules {
		if r.ID == id {
			b.rules = append(b.rules[:i], b.rules[i+1:]...)
			return nil
		}
	}
	return mail.ErrRuleNotFound
}

func ruleTestHandler(b mail.Backend) Handler {
	return Handler{mail: mailProviderFunc(func(context.Context) (mail.Backend, error) { return b, nil })}
}

// seededFilterBackend returns a backend pre-loaded with one rule that exercises a
// condition and several actions, so the mapping round-trip can be checked.
func seededFilterBackend() *filterBackend {
	return &filterBackend{
		nextID: 1,
		rules: []mail.MessageRule{{
			ID:          "rule-1",
			DisplayName: "from boss",
			Sequence:    1,
			Enabled:     true,
			Conditions: mail.RuleConditions{
				SubjectContains: []string{"urgent"},
				FromAddresses:   []mail.Address{{Name: "Boss", Email: "boss@example.com"}},
			},
			Actions: mail.RuleActions{
				MoveToFolder: "folder-123",
				MarkAsRead:   true,
			},
		}},
	}
}

func TestMeMailFoldersListMessageRules(t *testing.T) {
	h := ruleTestHandler(seededFilterBackend())

	res, err := h.MeMailFoldersListMessageRules(context.Background(), api.MeMailFoldersListMessageRulesParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	coll, ok := res.(*api.MicrosoftGraphMessageRuleCollectionResponseStatusCode)
	if !ok {
		t.Fatalf("response type = %T, want collection", res)
	}
	if len(coll.Response.Value) != 1 {
		t.Fatalf("rule count = %d, want 1", len(coll.Response.Value))
	}
	got := coll.Response.Value[0]
	if v, _ := got.DisplayName.Get(); v != "from boss" {
		t.Errorf("displayName = %q, want %q", v, "from boss")
	}
	if v, _ := got.Conditions.Get(); len(v.SubjectContains) != 1 || v.SubjectContains[0].Or("") != "urgent" {
		t.Errorf("subjectContains = %+v, want [urgent]", v.SubjectContains)
	}
	if v, _ := got.Actions.Get(); v.MoveToFolder.Or("") != "folder-123" || !v.MarkAsRead.Or(false) {
		t.Errorf("actions = %+v, want moveToFolder=folder-123 markAsRead=true", v)
	}
}

func TestMeMailFoldersGetMessageRules(t *testing.T) {
	h := ruleTestHandler(seededFilterBackend())

	res, err := h.MeMailFoldersGetMessageRules(context.Background(), api.MeMailFoldersGetMessageRulesParams{MessageRuleID: "rule-1"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := res.(*api.MicrosoftGraphMessageRuleStatusCode); !ok {
		t.Fatalf("response type = %T, want messageRule", res)
	}

	// A missing rule maps ErrRuleNotFound to a Graph 404.
	res, err = h.MeMailFoldersGetMessageRules(context.Background(), api.MeMailFoldersGetMessageRulesParams{MessageRuleID: "nope"})
	if err != nil {
		t.Fatalf("get missing: unexpected error %v", err)
	}
	errRes, ok := res.(*api.ErrorStatusCode)
	if !ok || errRes.StatusCode != http.StatusNotFound {
		t.Fatalf("missing rule response = %T (status %v), want 404 ErrorStatusCode", res, statusOf(res))
	}
}

func TestMeMailFoldersCreateMessageRules(t *testing.T) {
	b := &filterBackend{}
	h := ruleTestHandler(b)

	req := &api.MicrosoftGraphMessageRule{
		DisplayName: api.NewOptNilString("forward newsletters"),
		Sequence:    api.NewOptNilInt32(2),
		IsEnabled:   api.NewOptNilBool(true),
		Conditions: api.NewOptMicrosoftGraphMessageRulePredicates(api.MicrosoftGraphMessageRulePredicates{
			SenderContains: []api.NilString{api.NewNilString("newsletter")},
		}),
		Actions: api.NewOptMicrosoftGraphMessageRuleActions(api.MicrosoftGraphMessageRuleActions{
			ForwardTo: []api.MicrosoftGraphRecipient{{
				EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
					Address: api.NewOptNilString("me@example.com"),
				}),
			}},
		}),
	}
	res, err := h.MeMailFoldersCreateMessageRules(context.Background(), req, api.MeMailFoldersCreateMessageRulesParams{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created, ok := res.(*api.MicrosoftGraphMessageRuleStatusCode)
	if !ok || created.StatusCode != http.StatusCreated {
		t.Fatalf("create response = %T (status %v), want 201 messageRule", res, statusOf(res))
	}
	if id, _ := created.Response.ID.Get(); id == "" {
		t.Error("created rule has no backend-assigned ID")
	}

	// The backend now holds exactly the created rule, with the fields mapped through.
	if len(b.rules) != 1 {
		t.Fatalf("backend rule count = %d, want 1", len(b.rules))
	}
	stored := b.rules[0]
	if stored.DisplayName != "forward newsletters" || stored.Sequence != 2 || !stored.Enabled {
		t.Errorf("stored rule envelope = %+v", stored)
	}
	if len(stored.Conditions.SenderContains) != 1 || stored.Conditions.SenderContains[0] != "newsletter" {
		t.Errorf("stored senderContains = %v, want [newsletter]", stored.Conditions.SenderContains)
	}
	if len(stored.Actions.ForwardTo) != 1 || stored.Actions.ForwardTo[0].Email != "me@example.com" {
		t.Errorf("stored forwardTo = %+v, want me@example.com", stored.Actions.ForwardTo)
	}
}

// TestMeMailFoldersUpdateMessageRulesPatchMerges is the core PATCH-semantics test:
// a PATCH that sets only isEnabled must leave the rule's conditions and actions
// untouched (read-merge-write), not wipe them.
func TestMeMailFoldersUpdateMessageRulesPatchMerges(t *testing.T) {
	b := seededFilterBackend()
	h := ruleTestHandler(b)

	patch := &api.MicrosoftGraphMessageRule{IsEnabled: api.NewOptNilBool(false)}
	res, err := h.MeMailFoldersUpdateMessageRules(context.Background(), patch, api.MeMailFoldersUpdateMessageRulesParams{MessageRuleID: "rule-1"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, ok := res.(*api.MicrosoftGraphMessageRuleStatusCode); !ok {
		t.Fatalf("update response = %T, want messageRule", res)
	}

	stored := b.rules[0]
	if stored.Enabled {
		t.Error("isEnabled patch did not take effect")
	}
	// The omitted members must survive the partial update.
	if len(stored.Conditions.SubjectContains) != 1 || stored.Conditions.SubjectContains[0] != "urgent" {
		t.Errorf("conditions wiped by partial PATCH: %+v", stored.Conditions)
	}
	if stored.Actions.MoveToFolder != "folder-123" || !stored.Actions.MarkAsRead {
		t.Errorf("actions wiped by partial PATCH: %+v", stored.Actions)
	}

	// PATCH of a missing rule maps to 404.
	res, err = h.MeMailFoldersUpdateMessageRules(context.Background(), patch, api.MeMailFoldersUpdateMessageRulesParams{MessageRuleID: "nope"})
	if err != nil {
		t.Fatalf("update missing: unexpected error %v", err)
	}
	if errRes, ok := res.(*api.ErrorStatusCode); !ok || errRes.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing response = %T (status %v), want 404", res, statusOf(res))
	}
}

func TestMeMailFoldersDeleteMessageRules(t *testing.T) {
	b := seededFilterBackend()
	h := ruleTestHandler(b)

	res, err := h.MeMailFoldersDeleteMessageRules(context.Background(), api.MeMailFoldersDeleteMessageRulesParams{MessageRuleID: "rule-1"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := res.(*api.MeMailFoldersDeleteMessageRulesNoContent); !ok {
		t.Fatalf("delete response = %T, want NoContent", res)
	}
	if len(b.rules) != 0 {
		t.Errorf("rule not removed, %d remain", len(b.rules))
	}

	// Deleting a missing rule maps to 404.
	res, err = h.MeMailFoldersDeleteMessageRules(context.Background(), api.MeMailFoldersDeleteMessageRulesParams{MessageRuleID: "rule-1"})
	if err != nil {
		t.Fatalf("delete missing: unexpected error %v", err)
	}
	if errRes, ok := res.(*api.ErrorStatusCode); !ok || errRes.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing response = %T (status %v), want 404", res, statusOf(res))
	}
}

// TestMessageRulesNotImplemented verifies a backend without the filter capability
// yields 501 from every messageRules handler.
func TestMessageRulesNotImplemented(t *testing.T) {
	h := ruleTestHandler(bareMailBackend{})
	ctx := context.Background()

	if _, err := h.MeMailFoldersListMessageRules(ctx, api.MeMailFoldersListMessageRulesParams{}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("list err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeMailFoldersGetMessageRules(ctx, api.MeMailFoldersGetMessageRulesParams{MessageRuleID: "x"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("get err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeMailFoldersCreateMessageRules(ctx, &api.MicrosoftGraphMessageRule{}, api.MeMailFoldersCreateMessageRulesParams{}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("create err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeMailFoldersUpdateMessageRules(ctx, &api.MicrosoftGraphMessageRule{}, api.MeMailFoldersUpdateMessageRulesParams{MessageRuleID: "x"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("update err = %v, want ErrNotImplemented", err)
	}
	if _, err := h.MeMailFoldersDeleteMessageRules(ctx, api.MeMailFoldersDeleteMessageRulesParams{MessageRuleID: "x"}); !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("delete err = %v, want ErrNotImplemented", err)
	}
}

// writeOnlyFilterBackend implements FilterWriter but NOT FilterReader, to verify
// the update handler — which reads the existing rule to merge a partial PATCH —
// requires both capabilities and returns 501 when the reader is missing.
type writeOnlyFilterBackend struct {
	bareMailBackend
}

func (writeOnlyFilterBackend) CreateRule(_ context.Context, r mail.MessageRule) (mail.MessageRule, error) {
	return r, nil
}
func (writeOnlyFilterBackend) UpdateRule(_ context.Context, _ string, r mail.MessageRule) (mail.MessageRule, error) {
	return r, nil
}
func (writeOnlyFilterBackend) DeleteRule(context.Context, string) error { return nil }

// TestMeMailFoldersUpdateMessageRulesNeedsReader asserts PATCH yields 501 on a
// backend that can write but not read rules, since the partial-update merge must
// first read the existing rule.
func TestMeMailFoldersUpdateMessageRulesNeedsReader(t *testing.T) {
	h := ruleTestHandler(writeOnlyFilterBackend{})
	_, err := h.MeMailFoldersUpdateMessageRules(context.Background(), &api.MicrosoftGraphMessageRule{IsEnabled: api.NewOptNilBool(false)}, api.MeMailFoldersUpdateMessageRulesParams{MessageRuleID: "x"})
	if !errors.Is(err, ht.ErrNotImplemented) {
		t.Errorf("update without FilterReader err = %v, want ErrNotImplemented", err)
	}
}

// statusOf extracts a response's HTTP status for diagnostics, or -1.
func statusOf(res any) int {
	if e, ok := res.(*api.ErrorStatusCode); ok {
		return e.StatusCode
	}
	return -1
}
