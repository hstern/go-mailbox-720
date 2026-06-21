// Package sieve translates the neutral inbox-rule model (mail.MessageRule) to and
// from Sieve scripts (RFC 5228), the language both filter tiers carry: over JMAP
// for Sieve Scripts (RFC 9661, internal/sieve/jmap) and over ManageSieve (RFC 5804,
// go-managesieve). It is the shared core of MB720-19 chunk B/C; the per-tier
// adapters only move the resulting script text.
//
// Translation uses github.com/hstern/go-sieve for the typed AST, emitter, and
// parser. A Graph messageRule carries metadata Sieve has no native slot for — a
// stable id, a display name, an enabled flag — so each rule is emitted with a
// leading "# x-mailbox-rule: {json}" comment that stores the whole neutral rule,
// followed (only when the rule is enabled) by the executable "if" block the mail
// server actually runs on delivery. The comment is the canonical store: Decode
// reads rules back from it, which makes the round-trip lossless and represents
// disabled rules (a comment with no if) cleanly. A script with no such comments —
// one authored outside this server — is imported structurally from its if blocks
// as a best effort, with synthesized ids and names.
package sieve

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	gosieve "github.com/hstern/go-sieve"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

// metaPrefix tags the comment that carries a rule's neutral JSON, distinguishing it
// from any other comment in the script.
const metaPrefix = "x-mailbox-rule: "

// seenFlag is the IMAP \Seen flag set by the mark-as-read action (RFC 5232).
const seenFlag = `\Seen`

// Encode renders the rules as a Sieve script: each rule as its canonical metadata
// comment plus, when enabled, the executable if block. Rules are ordered by
// Sequence. The go-sieve emitter derives the leading require automatically.
func Encode(rules []mail.MessageRule) ([]byte, error) {
	ordered := append([]mail.MessageRule(nil), rules...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })

	cmds := make([]gosieve.Command, 0, len(ordered)*2)
	for _, r := range ordered {
		meta, err := json.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("sieve: encode rule metadata: %w", err)
		}
		cmds = append(cmds, &gosieve.Comment{Text: metaPrefix + string(meta)})
		if r.Enabled {
			cmds = append(cmds, buildIf(r))
		}
	}
	out, err := (&gosieve.Script{Commands: cmds}).Encode()
	if err != nil {
		return nil, fmt.Errorf("sieve: encode: %w", err)
	}
	return out, nil
}

// Decode reconstructs the rules from a Sieve script. It prefers the canonical
// x-mailbox-rule metadata comments (a lossless round-trip of an Encode result); a
// script that carries none is imported structurally from its top-level if blocks.
func Decode(b []byte) ([]mail.MessageRule, error) {
	script, err := gosieve.Parse(b, gosieve.KeepComments())
	if err != nil {
		return nil, fmt.Errorf("sieve: parse: %w", err)
	}

	var fromMeta []mail.MessageRule
	for _, cmd := range script.Commands {
		c, ok := cmd.(*gosieve.Comment)
		if !ok {
			continue
		}
		text, ok := strings.CutPrefix(strings.TrimSpace(c.Text), metaPrefix)
		if !ok {
			continue
		}
		var r mail.MessageRule
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			return nil, fmt.Errorf("sieve: decode rule metadata: %w", err)
		}
		fromMeta = append(fromMeta, r)
	}
	if len(fromMeta) > 0 {
		return fromMeta, nil
	}

	// No metadata: import a foreign script structurally from its if blocks.
	var rules []mail.MessageRule
	for _, cmd := range script.Commands {
		if ifc, ok := cmd.(*gosieve.If); ok {
			rules = append(rules, ifToRule(ifc, len(rules)))
		}
	}
	return rules, nil
}

// buildIf builds the executable if block for an (enabled) rule: its conditions as
// the test, its actions as the body.
func buildIf(r mail.MessageRule) *gosieve.If {
	return &gosieve.If{Test: buildTest(r.Conditions), Then: buildActions(r.Actions)}
}

// buildTest maps the neutral conditions onto a Sieve test. Each populated field is
// one test whose key list is OR-matched within the field; the fields are AND-ed via
// allof. Empty conditions become the always-true test (the rule fires on every
// message), matching an empty RuleConditions.
func buildTest(c mail.RuleConditions) gosieve.Test {
	var tests []gosieve.Test
	if len(c.SubjectContains) > 0 {
		tests = append(tests, &gosieve.HeaderTest{MatchType: gosieve.MatchContains, Headers: []string{"subject"}, Keys: c.SubjectContains})
	}
	if len(c.SenderContains) > 0 {
		tests = append(tests, &gosieve.HeaderTest{MatchType: gosieve.MatchContains, Headers: []string{"from"}, Keys: c.SenderContains})
	}
	if len(c.BodyContains) > 0 {
		tests = append(tests, &gosieve.BodyTest{MatchType: gosieve.MatchContains, Keys: c.BodyContains})
	}
	if len(c.FromAddresses) > 0 {
		tests = append(tests, &gosieve.AddressTest{MatchType: gosieve.MatchIs, Headers: []string{"from"}, Keys: emails(c.FromAddresses)})
	}
	if len(c.SentToAddresses) > 0 {
		tests = append(tests, &gosieve.AddressTest{MatchType: gosieve.MatchIs, Headers: []string{"to"}, Keys: emails(c.SentToAddresses)})
	}
	switch len(tests) {
	case 0:
		return &gosieve.True{}
	case 1:
		return tests[0]
	default:
		return &gosieve.AllOf{Tests: tests}
	}
}

// buildActions maps the neutral actions onto Sieve commands. Stop is emitted last so
// earlier actions always run. Forward keeps the original (redirect :copy); redirect
// does not.
func buildActions(a mail.RuleActions) []gosieve.Command {
	var cmds []gosieve.Command
	if a.MoveToFolder != "" {
		cmds = append(cmds, &gosieve.FileInto{Mailbox: a.MoveToFolder})
	}
	if a.CopyToFolder != "" {
		cmds = append(cmds, &gosieve.FileInto{Mailbox: a.CopyToFolder, Copy: true})
	}
	if a.MarkAsRead {
		cmds = append(cmds, &gosieve.AddFlag{Flags: []string{seenFlag}})
	}
	for _, addr := range a.ForwardTo {
		cmds = append(cmds, &gosieve.Redirect{Address: addr.Email, Copy: true})
	}
	for _, addr := range a.RedirectTo {
		cmds = append(cmds, &gosieve.Redirect{Address: addr.Email})
	}
	if a.Delete {
		cmds = append(cmds, &gosieve.Discard{})
	}
	if a.StopProcessingRules {
		cmds = append(cmds, &gosieve.Stop{})
	}
	return cmds
}

// ifToRule reconstructs a neutral rule from a top-level if block of a foreign script
// (no metadata comment), synthesizing the id/name/sequence from its position.
func ifToRule(ifc *gosieve.If, idx int) mail.MessageRule {
	r := mail.MessageRule{
		ID:          fmt.Sprintf("rule-%d", idx+1),
		DisplayName: fmt.Sprintf("Rule %d", idx+1),
		Sequence:    idx + 1,
		Enabled:     true,
	}
	conditionsFromTest(ifc.Test, &r.Conditions)
	actionsFromCommands(ifc.Then, &r.Actions)
	return r
}

// conditionsFromTest folds a Sieve test back into the neutral conditions, recursing
// through allof. Only the exact forms Encode emits are recognized — header/body
// :contains and address :is — so a foreign test with a different match-type (e.g.
// an exact header :is "from") is dropped rather than silently demoted to a different
// neutral condition. Everything outside the modeled vocabulary is ignored
// (best-effort import).
func conditionsFromTest(t gosieve.Test, c *mail.RuleConditions) {
	switch v := t.(type) {
	case *gosieve.AllOf:
		for _, sub := range v.Tests {
			conditionsFromTest(sub, c)
		}
	case *gosieve.HeaderTest:
		if v.MatchType != gosieve.MatchContains {
			return
		}
		switch firstLower(v.Headers) {
		case "subject":
			c.SubjectContains = append(c.SubjectContains, v.Keys...)
		case "from":
			c.SenderContains = append(c.SenderContains, v.Keys...)
		}
	case *gosieve.BodyTest:
		if v.MatchType != gosieve.MatchContains {
			return
		}
		c.BodyContains = append(c.BodyContains, v.Keys...)
	case *gosieve.AddressTest:
		if v.MatchType != gosieve.MatchIs {
			return
		}
		switch firstLower(v.Headers) {
		case "from":
			c.FromAddresses = append(c.FromAddresses, addresses(v.Keys)...)
		case "to":
			c.SentToAddresses = append(c.SentToAddresses, addresses(v.Keys)...)
		}
	}
}

// actionsFromCommands folds a Sieve action list back into the neutral actions.
func actionsFromCommands(cmds []gosieve.Command, a *mail.RuleActions) {
	for _, cmd := range cmds {
		switch v := cmd.(type) {
		case *gosieve.FileInto:
			if v.Copy {
				a.CopyToFolder = v.Mailbox
			} else {
				a.MoveToFolder = v.Mailbox
			}
		case *gosieve.AddFlag:
			if hasSeen(v.Flags) {
				a.MarkAsRead = true
			}
		case *gosieve.SetFlag:
			if hasSeen(v.Flags) {
				a.MarkAsRead = true
			}
		case *gosieve.Redirect:
			if v.Copy {
				a.ForwardTo = append(a.ForwardTo, mail.Address{Email: v.Address})
			} else {
				a.RedirectTo = append(a.RedirectTo, mail.Address{Email: v.Address})
			}
		case *gosieve.Discard:
			a.Delete = true
		case *gosieve.Stop:
			a.StopProcessingRules = true
		}
	}
}

func emails(as []mail.Address) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Email)
	}
	return out
}

func addresses(emails []string) []mail.Address {
	out := make([]mail.Address, 0, len(emails))
	for _, e := range emails {
		out = append(out, mail.Address{Email: e})
	}
	return out
}

func firstLower(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return strings.ToLower(ss[0])
}

func hasSeen(flags []string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, seenFlag) {
			return true
		}
	}
	return false
}
