package imap

import (
	managesieve "github.com/hstern/go-managesieve"

	"github.com/emersion/go-sasl"
)

// saslAdapter drives an emersion/go-sasl client through go-managesieve's
// AUTHENTICATE loop (MB720-60). The two interfaces are shaped alike (Start +
// Next), so the adapter's job is the framing difference: go-managesieve sends an
// initial response only when it is non-nil and encodes every SASL response as a
// base64 quoted string (RFC 5804 §2.1), so an empty response goes out as "" —
// which is exactly the empty continuation GSSAPI sends after the AP-REP (RFC 4752
// §3.1). The adapter normalises an empty initial response to nil (no inline
// response, the server then prompts with an empty challenge) and copies nil/empty
// responses to a non-nil empty slice so they are always framed, never dropped.
type saslAdapter struct{ c sasl.Client }

var _ managesieve.SASLClient = saslAdapter{}

// newSASLAdapter adapts a (single-use) emersion sasl.Client to a
// managesieve.SASLClient.
func newSASLAdapter(c sasl.Client) managesieve.SASLClient { return saslAdapter{c: c} }

func (a saslAdapter) Start() (string, []byte, error) {
	mech, ir, err := a.c.Start()
	if err != nil {
		return "", nil, err
	}
	if len(ir) == 0 {
		ir = nil
	}
	return mech, ir, nil
}

func (a saslAdapter) Next(challenge []byte) ([]byte, error) {
	resp, err := a.c.Next(challenge)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		resp = []byte{}
	}
	return resp, nil
}
