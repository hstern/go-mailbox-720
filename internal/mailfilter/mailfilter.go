// Package mailfilter holds the transport-neutral read-modify-write CRUD of inbox
// rules, shared by the JMAP (RFC 9661) and ManageSieve (RFC 5804) tiers of MB720-19.
// A mail backend adapts its transport to ScriptStore and delegates its
// mail.FilterReader / mail.FilterWriter methods to the functions here, so the
// rule-list manipulation, id assignment, and the last-write-wins whole-script
// rewrite live in one place rather than once per tier.
package mailfilter

import (
	"context"
	"fmt"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/sieve"
)

// ScriptStore is the transport surface the rule CRUD needs: read and replace the
// account's single active Sieve script as opaque text. Implementations carry one
// tier's connection — a shared JMAP session, or a live ManageSieve connection. Each
// CRUD call operates entirely on the one store it is given, so a tier that needs a
// live connection opens it for the span of the call and the read-modify-write happens
// over that one connection.
type ScriptStore interface {
	// ActiveScript returns the content of the single rules script this server
	// manages, or ok=false when it does not exist yet. Implementations read that one
	// script by name (not merely whatever script is active), so a pre-existing script
	// the user keeps active elsewhere is never read or overwritten.
	ActiveScript(ctx context.Context) (content string, ok bool, err error)
	// SetActiveContent stores content as the managed rules script and makes it the
	// account's active script.
	SetActiveContent(ctx context.Context, content string) error
}

// List decodes the active script into the neutral rule model; no active script means
// no rules.
func List(ctx context.Context, s ScriptStore) ([]mail.MessageRule, error) {
	content, ok, err := s.ActiveScript(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	rules, err := sieve.Decode([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("mailfilter: decode rules: %w", err)
	}
	return rules, nil
}

// Get returns the rule with the given id, or mail.ErrRuleNotFound.
func Get(ctx context.Context, s ScriptStore, id string) (mail.MessageRule, error) {
	rules, err := List(ctx, s)
	if err != nil {
		return mail.MessageRule{}, err
	}
	for _, r := range rules {
		if r.ID == id {
			return r, nil
		}
	}
	return mail.MessageRule{}, mail.ErrRuleNotFound
}

// Create appends rule with a fresh opaque id and returns it.
func Create(ctx context.Context, s ScriptStore, rule mail.MessageRule) (mail.MessageRule, error) {
	rules, err := List(ctx, s)
	if err != nil {
		return mail.MessageRule{}, err
	}
	rule.ID = freshID(rules)
	if rule.Sequence == 0 {
		rule.Sequence = maxSequence(rules) + 1
	}
	if err := write(ctx, s, append(rules, rule)); err != nil {
		return mail.MessageRule{}, err
	}
	return rule, nil
}

// Update replaces the rule with the given id, or returns mail.ErrRuleNotFound.
func Update(ctx context.Context, s ScriptStore, id string, rule mail.MessageRule) (mail.MessageRule, error) {
	rules, err := List(ctx, s)
	if err != nil {
		return mail.MessageRule{}, err
	}
	for i := range rules {
		if rules[i].ID == id {
			rule.ID = id
			rules[i] = rule
			if err := write(ctx, s, rules); err != nil {
				return mail.MessageRule{}, err
			}
			return rule, nil
		}
	}
	return mail.MessageRule{}, mail.ErrRuleNotFound
}

// Delete removes the rule with the given id, or returns mail.ErrRuleNotFound.
func Delete(ctx context.Context, s ScriptStore, id string) error {
	rules, err := List(ctx, s)
	if err != nil {
		return err
	}
	kept := make([]mail.MessageRule, 0, len(rules))
	for _, r := range rules {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(rules) {
		return mail.ErrRuleNotFound
	}
	return write(ctx, s, kept)
}

func write(ctx context.Context, s ScriptStore, rules []mail.MessageRule) error {
	content, err := sieve.Encode(rules)
	if err != nil {
		return fmt.Errorf("mailfilter: encode rules: %w", err)
	}
	return s.SetActiveContent(ctx, string(content))
}

// freshID returns the lowest unused "rule-N" id.
func freshID(rules []mail.MessageRule) string {
	used := make(map[string]bool, len(rules))
	for _, r := range rules {
		used[r.ID] = true
	}
	for n := 1; ; n++ {
		if id := fmt.Sprintf("rule-%d", n); !used[id] {
			return id
		}
	}
}

// maxSequence returns the highest Sequence among rules, or 0.
func maxSequence(rules []mail.MessageRule) int {
	max := 0
	for _, r := range rules {
		if r.Sequence > max {
			max = r.Sequence
		}
	}
	return max
}
