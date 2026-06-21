package revocation

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	caep "github.com/hstern/go-caep"
	risc "github.com/hstern/go-risc"
	secevent "github.com/hstern/go-secevent"
	ssf "github.com/hstern/go-ssf"
	subjectid "github.com/hstern/go-subjectid"
)

const testKID = "ssf-test-key"

// transmitter is a minimal Shared Signals transmitter for tests: it serves OIDC
// discovery + a JWKS and signs SETs with its key, so the receiver can fetch the
// key and verify a pushed SET end to end.
type transmitter struct {
	issuer string
	key    *rsa.PrivateKey
	signer *ssf.JOSESetSigner
}

func newTransmitter(t *testing.T) *transmitter {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tx := &transmitter{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   tx.issuer,
			"jwks_uri": tx.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: testKID, Algorithm: "RS256", Use: "sig",
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tx.issuer = srv.URL

	joseSigner, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: testKID}},
		(&jose.SignerOptions{}).WithType(jose.ContentType(ssf.SETMediaType)),
	)
	if err != nil {
		t.Fatal(err)
	}
	setSigner, err := ssf.NewJOSESetSigner(joseSigner)
	if err != nil {
		t.Fatal(err)
	}
	tx.signer = setSigner
	return tx
}

// signSET builds a SET carrying the given event for sub and returns its compact JWS.
func (tx *transmitter) signSET(t *testing.T, sub subjectid.IssSubID, iat time.Time, ev secevent.Event) string {
	t.Helper()
	events := secevent.Events{}
	if err := caep.Put(events, ev); err != nil {
		t.Fatalf("put event: %v", err)
	}
	set := secevent.SET{
		Issuer:   tx.issuer,
		IssuedAt: iat,
		JWTID:    "set-" + iat.Format(time.RFC3339Nano),
		Subject:  sub,
		Events:   events,
	}
	payload, err := set.Encode()
	if err != nil {
		t.Fatalf("encode SET: %v", err)
	}
	jws, err := tx.signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign SET: %v", err)
	}
	return jws
}

// TestReceiverDeliversCAEPSessionRevoked drives the full receive path: a signed
// CAEP session-revoked SET is POSTed to the push handler, which fetches the
// transmitter's JWKS, verifies the JWS, and delivers the payload to the store.
func TestReceiverDeliversCAEPSessionRevoked(t *testing.T) {
	tx := newTransmitter(t)
	store := NewStore()
	h, err := Handler([]string{tx.issuer}, store)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	alice := subjectid.IssSubID{Iss: tx.issuer, Sub: "alice"}
	eventTime := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	ev := caep.SessionRevoked{Base: caep.Base{EventTimestamp: secevent.NewNumericDate(eventTime)}}
	jws := tx.signSET(t, alice, eventTime.Add(time.Minute), ev)

	rec := postSET(t, h, jws)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push status = %d (body %q), want 202", rec.Code, rec.Body.String())
	}

	// A token issued at-or-before the event timestamp is revoked; one issued after
	// survives.
	if !store.Revoked(alice, eventTime, "") {
		t.Error("token at the event timestamp must be revoked after a session-revoked SET")
	}
	if store.Revoked(alice, eventTime.Add(time.Hour), "") {
		t.Error("token issued after the event timestamp must survive")
	}
}

// TestReceiverRejectsBadSignature ensures a SET signed by an unknown key is
// rejected (400) and applies no revocation.
func TestReceiverRejectsBadSignature(t *testing.T) {
	tx := newTransmitter(t)
	store := NewStore()
	h, err := Handler([]string{tx.issuer}, store)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	// A second transmitter whose key the receiver does not know.
	other := newTransmitter(t)
	alice := subjectid.IssSubID{Iss: tx.issuer, Sub: "alice"}
	ev := caep.SessionRevoked{Base: caep.Base{EventTimestamp: secevent.NewNumericDate(time.Now())}}
	jws := other.signSET(t, alice, time.Now(), ev)

	rec := postSET(t, h, jws)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("push status = %d, want 400 for an unverifiable SET", rec.Code)
	}
	if store.Revoked(alice, time.Now(), "") {
		t.Error("an unverifiable SET must not revoke anything")
	}
}

// TestDeliverSETRISCAccountDisabled exercises a RISC account-disabled event
// directly through DeliverSET on the (already-verified) payload bytes. The SET is
// built with the typed go-risc producer API — the same construction path a real
// transmitter uses — then encoded to the on-the-wire bytes DeliverSET parses.
func TestDeliverSETRISCAccountDisabled(t *testing.T) {
	store := NewStore()
	iat := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	bob := subjectid.IssSubID{Iss: "https://idp.example", Sub: "bob"}

	payload := riscAccountDisabledSET(t, bob, iat)
	if err := store.DeliverSET(context.Background(), payload); err != nil {
		t.Fatalf("DeliverSET: %v", err)
	}
	// RISC events carry no event timestamp, so the SET's iat is the revocation time.
	if !store.Revoked(bob, iat, "") {
		t.Error("an account-disabled SET must revoke tokens at-or-before the SET iat")
	}
	if store.Revoked(bob, iat.Add(time.Hour), "") {
		t.Error("a token issued after the SET iat must survive")
	}
}

// TestDeliverSETNonIssSubNoOp asserts a SET whose subject is not an iss_sub
// identifier is accepted but revokes nothing.
func TestDeliverSETNonIssSubNoOp(t *testing.T) {
	store := NewStore()
	events := secevent.Events{}
	if err := caep.Put(events, caep.SessionRevoked{}); err != nil {
		t.Fatalf("put event: %v", err)
	}
	// An email Subject Identifier — a valid SubjectIdentifier, but not an IssSubID.
	set := secevent.SET{
		Issuer:   "https://idp.example",
		IssuedAt: time.Now(),
		JWTID:    "set-email",
		Subject:  subjectid.EmailID{Email: "carol@example.com"},
		Events:   events,
	}
	payload, err := set.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := store.DeliverSET(context.Background(), payload); err != nil {
		t.Errorf("DeliverSET on a non-iss_sub subject must accept (no-op), got %v", err)
	}
}

// TestDeliverSETMalformedRejected asserts a non-SET payload is rejected.
func TestDeliverSETMalformedRejected(t *testing.T) {
	store := NewStore()
	if err := store.DeliverSET(context.Background(), []byte("not a set")); err == nil {
		t.Error("DeliverSET on malformed payload must return an error")
	}
}

// riscAccountDisabledSET builds a SET carrying a RISC account-disabled event for
// sub, issued at iat, via the typed go-risc producer API (NewAccountDisabled +
// risc.AddTo). The event carries its own subject (RISC puts the subject inside
// the event, distinct from the SET sub_id); the constructor sets it from sub.
func riscAccountDisabledSET(t *testing.T, sub subjectid.IssSubID, iat time.Time) []byte {
	t.Helper()
	events := secevent.Events{}
	if err := risc.AddTo(events, risc.NewAccountDisabled(sub)); err != nil {
		t.Fatalf("add event: %v", err)
	}
	set := secevent.SET{
		Issuer:   sub.Iss,
		IssuedAt: iat,
		JWTID:    "set-risc",
		Subject:  sub,
		Events:   events,
	}
	b, err := set.Encode()
	if err != nil {
		t.Fatalf("encode SET: %v", err)
	}
	return b
}

// postSET POSTs a compact-JWS SET to the push handler with the RFC 8935 media type.
func postSET(t *testing.T, h http.Handler, jws string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ssf/events", strings.NewReader(jws))
	req.Header.Set("Content-Type", "application/"+ssf.SETMediaType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
