package revocation

import (
	"testing"
	"time"

	subjectid "github.com/hstern/go-subjectid"
)

func sub(s string) subjectid.IssSubID {
	return subjectid.IssSubID{Iss: "https://idp.example", Sub: s}
}

func TestStoreRevokeToken(t *testing.T) {
	s := NewStore()
	alice := sub("alice")
	s.RevokeToken("jti-1")

	if !s.Revoked(alice, time.Now(), "jti-1") {
		t.Error("a revoked jti must be reported revoked")
	}
	if s.Revoked(alice, time.Now(), "jti-2") {
		t.Error("an unrelated jti must not be revoked by another jti's revocation")
	}
	// An empty jti never matches the token set, and with no subject revocation the
	// token is live.
	if s.Revoked(alice, time.Now(), "") {
		t.Error("empty jti with no subject revocation must not be revoked")
	}
	// RevokeToken("") is a no-op and must not create a catch-all revocation.
	s.RevokeToken("")
	if s.Revoked(alice, time.Now(), "") {
		t.Error("RevokeToken(\"\") must be a no-op")
	}
}

func TestStoreRevokeSubjectIssuedAtBoundary(t *testing.T) {
	s := NewStore()
	alice := sub("alice")
	T := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	s.RevokeSubject(alice, T)

	tests := []struct {
		name     string
		issuedAt time.Time
		want     bool
	}{
		{"issued before T is revoked", T.Add(-time.Hour), true},
		{"issued exactly at T is revoked (at-or-before)", T, true},
		{"issued after T survives", T.Add(time.Hour), false},
		{"zero issued-at is revoked (position unprovable)", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Revoked(alice, tc.issuedAt, ""); got != tc.want {
				t.Errorf("Revoked(issuedAt=%v) = %v, want %v", tc.issuedAt, got, tc.want)
			}
		})
	}

	// A different subject is untouched.
	if s.Revoked(sub("bob"), T.Add(-time.Hour), "") {
		t.Error("revoking alice must not revoke bob")
	}
}

func TestStoreRevokeSubjectLatestWins(t *testing.T) {
	s := NewStore()
	alice := sub("alice")
	early := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	late := time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC)

	// Apply the later revocation first, then an earlier one: the window must not
	// narrow back to the earlier time.
	s.RevokeSubject(alice, late)
	s.RevokeSubject(alice, early)

	between := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	if !s.Revoked(alice, between, "") {
		t.Error("a token issued before the latest revocation time must stay revoked (latest-wins)")
	}
}

func TestStoreRevokeSubjectZeroAtMeansNow(t *testing.T) {
	s := NewStore()
	alice := sub("alice")
	before := time.Now()
	s.RevokeSubject(alice, time.Time{}) // zero -> now
	after := time.Now()

	// A token issued before the revocation call is revoked.
	if !s.Revoked(alice, before.Add(-time.Second), "") {
		t.Error("token issued before a now-revocation must be revoked")
	}
	// A token issued safely after the revocation call survives.
	if s.Revoked(alice, after.Add(time.Hour), "") {
		t.Error("token issued after a now-revocation must survive")
	}
}
