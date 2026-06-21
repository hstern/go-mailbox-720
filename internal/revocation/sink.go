package revocation

import (
	"context"
	"time"

	// Importing go-caep and go-risc also registers their event decoders with
	// go-secevent (via their init funcs), so secevent.Parse below decodes their
	// events to typed values rather than leaving them as raw bytes.
	caep "github.com/hstern/go-caep"
	risc "github.com/hstern/go-risc"
	secevent "github.com/hstern/go-secevent"
)

// riscRevocationEvents are the RISC account-lifecycle event-type URIs that, like a
// CAEP session-revoked, kill the subject's existing tokens. They carry no per-event
// timestamp, so the SET's iat is used as the revocation time.
var riscRevocationEvents = []string{
	risc.AccountDisabledURI,
	risc.AccountPurgedURI,
	risc.CredentialCompromiseURI,
}

// DeliverSET decodes a verified SET's payload and applies the revocations it
// carries to the Store, making *Store a receiver.Sink. The push handler has already
// verified the JWS (signature, typ, alg) before calling this, so the bytes are
// trusted; this only parses the claims set and maps known events to revocations.
//
// A SET whose subject is not an RFC 9493 iss_sub identifier (a different
// SubjectIdentifier format, or none) is accepted but no-ops: this server keys
// mailboxes by (iss, sub) and cannot act on another identifier shape. A parse
// failure is returned so the push handler answers an error per RFC 8935.
func (s *Store) DeliverSET(_ context.Context, payload []byte) error {
	set, err := secevent.Parse(payload)
	if err != nil {
		return err
	}
	sub, ok := set.IssSub()
	if !ok {
		// Unknown/absent subject format: nothing to revoke here. Accept (the SET
		// was valid) rather than reject, so the transmitter does not retry.
		return nil
	}

	// CAEP session-revoked: revoke at the event's own timestamp when present, else
	// the SET issuance time (CAEP §3.2).
	if ev, present, evErr := set.Events.Typed(caep.EventSessionRevoked); evErr == nil && present {
		if sr, isSR := ev.(caep.SessionRevoked); isSR {
			s.RevokeSubject(sub, sessionRevokedTime(sr, set.IssuedAt))
		}
	}

	// RISC account-lifecycle events: these have no per-event timestamp, so the
	// SET's iat is the revocation time.
	for _, uri := range riscRevocationEvents {
		if _, present, evErr := set.Events.Typed(uri); evErr == nil && present {
			s.RevokeSubject(sub, set.IssuedAt)
		}
	}
	return nil
}

// sessionRevokedTime resolves a CAEP session-revoked event's effective revocation
// time: its event_timestamp if carried, else the SET's iat as a fallback.
func sessionRevokedTime(sr caep.SessionRevoked, issuedAt time.Time) time.Time {
	if ts := sr.EventTimestamp; ts != nil {
		return ts.Time
	}
	return issuedAt
}
