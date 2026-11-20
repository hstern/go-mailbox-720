package imap

import (
	"github.com/emersion/go-sasl"
	managesieve "github.com/hstern/go-managesieve"
)

// newSASLAdapter adapts an emersion/go-sasl client to go-managesieve's
// AUTHENTICATE loop (MB720-60). The two interfaces are structurally identical
// (Start + Next), and go-managesieve's framing already does what the mechanisms
// need, so no translation is required:
//   - an initial response is sent inline only when non-nil, so a mechanism's
//     nil ("no initial response") is distinguished from an empty one, which goes
//     out as "" (RFC 5804 §2.1; e.g. EXTERNAL with an empty authzid);
//   - every Next response, nil or empty included, is base64-quoted, so the empty
//     continuation GSSAPI sends after the AP-REP (RFC 4752 §3.1) is framed as "".
//
// The conversion exists to make that contract explicit and compile-checked; the
// client is single-use, so callers build a fresh one per connection.
func newSASLAdapter(c sasl.Client) managesieve.SASLClient { return c }
