package jmap

import (
	"context"
	"errors"
	"fmt"

	port "github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/sieve"
	sievejmap "github.com/hstern/go-mailbox-720/internal/sieve/jmap"
)

// This file implements the optional mail.FilterReader / mail.FilterWriter
// capabilities on the JMAP mail backend (MB720-19 chunk C). Inbox rules live in the
// account's single active Sieve script, managed over JMAP for Sieve Scripts
// (RFC 9661) on the same session as the mail backend; the neutral mail.MessageRule
// model is translated to and from the script text by internal/sieve.
//
// Every mutation is a read-modify-write of the whole active script: read it, change
// the one rule, re-encode and re-publish all rules. There is deliberately no JMAP
// ifInState guard, so concurrent writers are last-write-wins — a second writer can
// overwrite a first writer's change. That is an accepted trade-off for a
// single-mailbox gateway, where concurrent edits to the same mailbox's rules are not
// expected; an ifInState retry loop can be added if that assumption stops holding.

// rulesScriptName is the Sieve script this server owns and keeps active to hold the
// mailbox's inbox rules.
const rulesScriptName = "mailboxd"

var (
	_ port.FilterReader = (*Client)(nil)
	_ port.FilterWriter = (*Client)(nil)
)

// sieveClient wraps this backend's JMAP session as a SieveScript transport, sharing
// the same connection and account session. It reports mail.ErrFiltersUnsupported
// when the server does not advertise the Sieve capability.
func (cl *Client) sieveClient() (*sievejmap.Client, error) {
	sc, err := sievejmap.FromClient(cl.c)
	if err != nil {
		if errors.Is(err, sievejmap.ErrNoSieveAccount) {
			return nil, port.ErrFiltersUnsupported
		}
		return nil, fmt.Errorf("jmap: sieve: %w", err)
	}
	return sc, nil
}

// ListRules decodes the account's active Sieve script into the neutral rule model.
// No active script means no rules.
func (cl *Client) ListRules(ctx context.Context) ([]port.MessageRule, error) {
	sc, err := cl.sieveClient()
	if err != nil {
		return nil, err
	}
	return rulesFrom(ctx, sc)
}

// rulesFrom reads and decodes the active script via an already-resolved transport,
// so a read-modify-write op resolves the Sieve client once and shares it.
func rulesFrom(ctx context.Context, sc *sievejmap.Client) ([]port.MessageRule, error) {
	_, content, ok, err := sc.ActiveScript(ctx)
	if err != nil {
		return nil, fmt.Errorf("jmap: read active sieve script: %w", err)
	}
	if !ok {
		return nil, nil
	}
	rules, err := sieve.Decode([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("jmap: decode rules: %w", err)
	}
	return rules, nil
}

// GetRule returns the rule with the given id, or mail.ErrRuleNotFound.
func (cl *Client) GetRule(ctx context.Context, id string) (port.MessageRule, error) {
	rules, err := cl.ListRules(ctx)
	if err != nil {
		return port.MessageRule{}, err
	}
	for _, r := range rules {
		if r.ID == id {
			return r, nil
		}
	}
	return port.MessageRule{}, port.ErrRuleNotFound
}

// CreateRule appends rule to the active script with a fresh opaque id, returning it.
func (cl *Client) CreateRule(ctx context.Context, rule port.MessageRule) (port.MessageRule, error) {
	sc, err := cl.sieveClient()
	if err != nil {
		return port.MessageRule{}, err
	}
	rules, err := rulesFrom(ctx, sc)
	if err != nil {
		return port.MessageRule{}, err
	}
	rule.ID = freshRuleID(rules)
	if rule.Sequence == 0 {
		rule.Sequence = maxSequence(rules) + 1
	}
	if err := writeRules(ctx, sc, append(rules, rule)); err != nil {
		return port.MessageRule{}, err
	}
	return rule, nil
}

// UpdateRule replaces the rule with the given id, or returns mail.ErrRuleNotFound.
func (cl *Client) UpdateRule(ctx context.Context, id string, rule port.MessageRule) (port.MessageRule, error) {
	sc, err := cl.sieveClient()
	if err != nil {
		return port.MessageRule{}, err
	}
	rules, err := rulesFrom(ctx, sc)
	if err != nil {
		return port.MessageRule{}, err
	}
	for i := range rules {
		if rules[i].ID == id {
			rule.ID = id
			rules[i] = rule
			if err := writeRules(ctx, sc, rules); err != nil {
				return port.MessageRule{}, err
			}
			return rule, nil
		}
	}
	return port.MessageRule{}, port.ErrRuleNotFound
}

// DeleteRule removes the rule with the given id, or returns mail.ErrRuleNotFound.
func (cl *Client) DeleteRule(ctx context.Context, id string) error {
	sc, err := cl.sieveClient()
	if err != nil {
		return err
	}
	rules, err := rulesFrom(ctx, sc)
	if err != nil {
		return err
	}
	kept := make([]port.MessageRule, 0, len(rules))
	for _, r := range rules {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(rules) {
		return port.ErrRuleNotFound
	}
	return writeRules(ctx, sc, kept)
}

// writeRules encodes the rules to a Sieve script and publishes it as the account's
// active script via an already-resolved transport.
func writeRules(ctx context.Context, sc *sievejmap.Client, rules []port.MessageRule) error {
	content, err := sieve.Encode(rules)
	if err != nil {
		return fmt.Errorf("jmap: encode rules: %w", err)
	}
	if err := sc.SetActiveContent(ctx, rulesScriptName, string(content)); err != nil {
		return fmt.Errorf("jmap: publish rules script: %w", err)
	}
	return nil
}

// freshRuleID returns the lowest unused "rule-N" id.
func freshRuleID(rules []port.MessageRule) string {
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
func maxSequence(rules []port.MessageRule) int {
	max := 0
	for _, r := range rules {
		if r.Sequence > max {
			max = r.Sequence
		}
	}
	return max
}
