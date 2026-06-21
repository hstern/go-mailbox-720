// Package revocation receives OpenID Shared Signals Security Event Tokens (SETs)
// and terminates the sessions they revoke. A long-lived bearer token is only
// re-validated at its own expiry; without this, a revoked token stays usable
// until then. CAEP session-revoked and RISC account-lifecycle events let the
// issuer push a revocation, which the auth middleware then enforces on the next
// request (MB720-18).
//
// The package has three parts: a Store (the revocation state and the enforcement
// predicate the auth middleware calls), a Sink (decodes a verified SET into Store
// mutations), and a Handler (the RFC 8935 push endpoint that verifies the SET's
// JWS against the issuers' JWKS before handing the payload to the Sink).
//
// Stream auto-registration (go-ssf client.CreateConfig) is deliberately NOT built
// here: it requires a transmitter advertising /.well-known/ssf-configuration plus
// management credentials, and is untestable without one. A deployment registers
// its receiver with the transmitter out of band for now.
package revocation

import (
	"sync"
	"time"

	subjectid "github.com/hstern/go-subjectid"
)

// Store holds revocation state in memory and answers the enforcement question the
// auth middleware asks on every request: is this token revoked? It is safe for
// concurrent use.
//
// Two kinds of revocation are tracked:
//
//   - per-token (jti): the exact token is dead. RFC 8417 SETs carry no jti of the
//     revoked token in the CAEP/RISC events used here, but RevokeToken supports a
//     transmitter (or a future poll path) that revokes a specific token.
//   - per-subject (iss_sub) at a time T: session-revoked semantics — every token
//     for that subject issued at-or-before T is dead (CAEP §3.2, RISC account
//     events). Later-issued tokens (a fresh login after the revocation) survive.
//
// This is a single-process in-memory store with no eviction: entries accumulate
// for the process lifetime. For the single-mailbox deployment that is fine; a
// multi-tenant or long-running deployment wants a bounded/persistent store, noted
// as follow-up.
type Store struct {
	mu       sync.RWMutex
	subjects map[subjectid.IssSubID]time.Time // subject -> latest revocation time
	tokens   map[string]struct{}              // revoked jti set
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		subjects: make(map[subjectid.IssSubID]time.Time),
		tokens:   make(map[string]struct{}),
	}
}

// RevokeSubject records that sub's sessions are revoked as of at: every token for
// sub issued at-or-before at is thereafter revoked. A zero at means "now" (the
// event carried no timestamp). Latest-wins: a later revocation time supersedes an
// earlier one, so a re-revocation never narrows the window.
func (s *Store) RevokeSubject(sub subjectid.IssSubID, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.subjects[sub]; !ok || at.After(prev) {
		s.subjects[sub] = at
	}
}

// RevokeToken records that the token with the given jti is revoked outright.
func (s *Store) RevokeToken(jti string) {
	if jti == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[jti] = struct{}{}
}

// Revoked reports whether a token presented for sub, issued at issuedAt and bearing
// the given jti, is revoked. A revoked jti is always revoked. A subject revoked at
// time T revokes any token issued at-or-before T (!issuedAt.After(T)); a token whose
// issuedAt is zero (the iat was absent or unreadable) is treated as revoked whenever
// the subject has any revocation, since its position relative to T cannot be proven.
func (s *Store) Revoked(sub subjectid.IssSubID, issuedAt time.Time, jti string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if jti != "" {
		if _, ok := s.tokens[jti]; ok {
			return true
		}
	}
	revokedAt, ok := s.subjects[sub]
	if !ok {
		return false
	}
	if issuedAt.IsZero() {
		return true
	}
	return !issuedAt.After(revokedAt)
}
