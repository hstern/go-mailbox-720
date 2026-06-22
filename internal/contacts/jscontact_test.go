package contacts

import (
	"testing"

	"github.com/hstern/go-jscontact"
)

// vCard "home" is JSContact's "private" context. A contact read from a real
// CardDAV vCard (TYPE=home) carries the "private" context via the go-jscontact
// bridge; the Graph-facing type label must still report "home" so the server's
// phone routing (home→homePhones) and email labels match. This pins the
// normalization that a fake built only via NewPhone(_, "home") would not catch.
func TestEmailPhoneTypeNormalizesPrivateToHome(t *testing.T) {
	// Bridge-shaped values: the "private" context as a real vCard import produces.
	if got := EmailType(jscontact.EmailAddress{Address: "a@h.example", Contexts: map[string]bool{"private": true}}); got != "home" {
		t.Errorf(`EmailType(private) = %q, want "home"`, got)
	}
	if got := PhoneType(jscontact.Phone{Number: "+1", Contexts: map[string]bool{"private": true}}); got != "home" {
		t.Errorf(`PhoneType(private) = %q, want "home"`, got)
	}
	// "work" passes through unchanged; the "mobile" feature surfaces as "cell".
	if got := EmailType(jscontact.EmailAddress{Contexts: map[string]bool{"work": true}}); got != "work" {
		t.Errorf(`EmailType(work) = %q, want "work"`, got)
	}
	if got := PhoneType(jscontact.Phone{Features: map[string]bool{"mobile": true}}); got != "cell" {
		t.Errorf(`PhoneType(mobile) = %q, want "cell"`, got)
	}
}

// Builder + projection round-trip: NewEmail/NewPhone("home") store the RFC-9553
// "private" context but the type label reads back as "home".
func TestNewEmailPhoneHomeRoundTrip(t *testing.T) {
	e := NewEmail("a@h.example", "home")
	if !e.Contexts["private"] {
		t.Errorf("NewEmail(home) contexts = %v, want private set", e.Contexts)
	}
	if got := EmailType(e); got != "home" {
		t.Errorf("EmailType = %q, want home", got)
	}
	p := NewPhone("+1-555-0100", "home")
	if !p.Contexts["private"] {
		t.Errorf("NewPhone(home) contexts = %v, want private set", p.Contexts)
	}
	if got := PhoneType(p); got != "home" {
		t.Errorf("PhoneType = %q, want home", got)
	}
}
