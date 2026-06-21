package sieve

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

// sampleRules exercises the full MVP vocabulary: multiple condition fields, every
// action, an enabled and a disabled rule.
func sampleRules() []mail.MessageRule {
	return []mail.MessageRule{
		{
			ID: "r1", DisplayName: "From boss", Sequence: 1, Enabled: true,
			Conditions: mail.RuleConditions{
				SubjectContains: []string{"urgent", "asap"},
				FromAddresses:   []mail.Address{{Email: "boss@example.com"}},
			},
			Actions: mail.RuleActions{MoveToFolder: "Priority", MarkAsRead: true, StopProcessingRules: true},
		},
		{
			ID: "r2", DisplayName: "Newsletters", Sequence: 2, Enabled: true,
			Conditions: mail.RuleConditions{SenderContains: []string{"newsletter"}, BodyContains: []string{"unsubscribe"}},
			Actions:    mail.RuleActions{CopyToFolder: "Lists", ForwardTo: []mail.Address{{Email: "me@example.com"}}},
		},
		{
			ID: "r3", DisplayName: "Disabled spam rule", Sequence: 3, Enabled: false,
			Conditions: mail.RuleConditions{SentToAddresses: []mail.Address{{Email: "alias@example.com"}}},
			Actions:    mail.RuleActions{Delete: true, RedirectTo: []mail.Address{{Email: "quarantine@example.com"}}},
		},
	}
}

func TestRoundTripLossless(t *testing.T) {
	in := sampleRules()
	script, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(script)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v\n--- script ---\n%s", in, out, script)
	}
}

func TestEncodedSieveIsExecutable(t *testing.T) {
	script, err := Encode(sampleRules())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	s := string(script)
	// The emitter must derive the require for every extension the actions use.
	for _, cap := range []string{"fileinto", "imap4flags", "body", "copy"} {
		if !strings.Contains(s, cap) {
			t.Errorf("require is missing %q\n%s", cap, s)
		}
	}
	// Spot-check the executable projection of the enabled rules.
	wants := []string{
		`header :contains "subject"`,
		`fileinto "Priority"`,
		`addflag "\\Seen"`,
		`stop;`,
		`address "from"`, // :is is the default match-type, omitted from canonical output
		`redirect :copy "me@example.com"`,
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("encoded script missing %q\n%s", w, s)
		}
	}
}

func TestDisabledRuleOmitsIfButSurvives(t *testing.T) {
	script, err := Encode(sampleRules())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// The disabled rule contributes a metadata comment (which carries its full JSON,
	// quarantine address included) but no executable block, so its destructive actions
	// never run. The check therefore targets the executable Sieve verb, not the
	// address text — the latter legitimately appears in the canonical comment.
	s := string(script)
	if strings.Contains(s, `redirect "quarantine@example.com"`) {
		t.Errorf("disabled rule leaked an executable redirect:\n%s", s)
	}
	if strings.Count(s, "if ") != 2 { // only the two enabled rules emit a block
		t.Errorf("want 2 if blocks (enabled rules only), got %d\n%s", strings.Count(s, "if "), s)
	}
	// But it round-trips: the disabled rule comes back with Enabled=false intact.
	out, err := Decode(script)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var r3 *mail.MessageRule
	for i := range out {
		if out[i].ID == "r3" {
			r3 = &out[i]
		}
	}
	if r3 == nil || r3.Enabled || !r3.Actions.Delete {
		t.Errorf("disabled rule did not survive round-trip: %+v", r3)
	}
}

func TestStructuralImportOfForeignScript(t *testing.T) {
	// A hand-authored script with no x-mailbox-rule metadata is imported from its
	// if blocks, with synthesized ids/names.
	const foreign = `require ["fileinto", "imap4flags"];
if header :contains "subject" "invoice" {
    fileinto "Finance";
    addflag "\\Seen";
}
if address :is "to" "support@example.com" {
    redirect "team@example.com";
}
`
	rules, err := Decode([]byte(foreign))
	if err != nil {
		t.Fatalf("Decode foreign: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("imported %d rules, want 2", len(rules))
	}
	r0 := rules[0]
	if r0.ID != "rule-1" || !r0.Enabled || r0.Sequence != 1 {
		t.Errorf("rule[0] envelope = %+v", r0)
	}
	if len(r0.Conditions.SubjectContains) != 1 || r0.Conditions.SubjectContains[0] != "invoice" {
		t.Errorf("rule[0] subject = %v", r0.Conditions.SubjectContains)
	}
	if r0.Actions.MoveToFolder != "Finance" || !r0.Actions.MarkAsRead {
		t.Errorf("rule[0] actions = %+v", r0.Actions)
	}
	r1 := rules[1]
	if len(r1.Conditions.SentToAddresses) != 1 || r1.Conditions.SentToAddresses[0].Email != "support@example.com" {
		t.Errorf("rule[1] to = %v", r1.Conditions.SentToAddresses)
	}
	if len(r1.Actions.RedirectTo) != 1 || r1.Actions.RedirectTo[0].Email != "team@example.com" {
		t.Errorf("rule[1] redirect = %v", r1.Actions.RedirectTo)
	}
}

func TestStructuralImportSkipsUnrecognizedMatchType(t *testing.T) {
	// An exact header match (header :is) is not a form this translator emits; it must
	// be dropped on import, not mis-read as a substring (SenderContains) match.
	const foreign = `if header :is "from" "boss@example.com" { discard; }`
	rules, err := Decode([]byte(foreign))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	if len(rules[0].Conditions.SenderContains) != 0 || len(rules[0].Conditions.FromAddresses) != 0 {
		t.Errorf("header :is must not import as a condition: %+v", rules[0].Conditions)
	}
	if !rules[0].Actions.Delete { // the action still imports
		t.Errorf("action should import: %+v", rules[0].Actions)
	}
}

func TestEncodeEmptyConditionsIsAlwaysTrue(t *testing.T) {
	rules := []mail.MessageRule{{
		ID: "catch-all", Sequence: 1, Enabled: true,
		Actions: mail.RuleActions{MoveToFolder: "Archive"},
	}}
	script, err := Encode(rules)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(script), "if true") {
		t.Errorf("empty conditions should emit `if true`:\n%s", script)
	}
}
