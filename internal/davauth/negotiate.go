package davauth

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	saslgssapi "github.com/hstern/go-sasl-gssapi"
	"github.com/hstern/krb5/client"
	"github.com/hstern/krb5/credentials"
	"github.com/hstern/krb5/spnego"

	"github.com/emersion/go-webdav"
)

// TokenSource yields the base64 SPNEGO token for one HTTP request. Every call
// must return a NEW token: a Kerberos acceptor with a replay cache rejects a
// second request that presents an AP-REQ it has already seen.
type TokenSource func() (string, error)

// NegotiateHTTPClient wraps c so every request it performs carries a freshly
// minted "Authorization: Negotiate <token>" header (RFC 4559). A nil c defaults
// to http.DefaultClient. It mirrors BearerHTTPClient for the second per-identity
// mechanism (MB720-47): each request to a SPNEGO/Kerberos-fronted DAV backend
// carries a GSSAPI credential minted for the authenticated user. The token is
// presented bearer-style: only the initial token is sent and the server's
// mutual-auth reply is not processed.
//
// When c is an *http.Client the token is minted in its Transport, i.e. once per
// network round trip, so a followed redirect (net/http copies the Authorization
// header across same-host redirects) gets its own token instead of replaying the
// first. Any other webdav.HTTPClient is wrapped at Do, once per call.
func NegotiateHTTPClient(c webdav.HTTPClient, src TokenSource) webdav.HTTPClient {
	if c == nil {
		c = http.DefaultClient
	}
	if hc, ok := c.(*http.Client); ok {
		cp := *hc
		rt := cp.Transport
		if rt == nil {
			rt = http.DefaultTransport
		}
		cp.Transport = &negotiateTransport{rt: rt, src: src}
		return &cp
	}
	return &negotiateHTTPClient{c: c, src: src}
}

// negotiateRequest returns a clone of req carrying a new Negotiate token.
func negotiateRequest(req *http.Request, src TokenSource) (*http.Request, error) {
	tok, err := src()
	if err != nil {
		return nil, fmt.Errorf("negotiate: mint token: %w", err)
	}
	// Clone so the caller's request is not mutated, as RoundTrip requires.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Negotiate "+tok)
	return req, nil
}

type negotiateTransport struct {
	rt  http.RoundTripper
	src TokenSource
}

func (n *negotiateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req, err := negotiateRequest(req, n.src)
	if err != nil {
		return nil, err
	}
	return n.rt.RoundTrip(req)
}

type negotiateHTTPClient struct {
	c   webdav.HTTPClient
	src TokenSource
}

func (n *negotiateHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req, err := negotiateRequest(req, n.src)
	if err != nil {
		return nil, err
	}
	return n.c.Do(req)
}

// KerberosTokenSource builds a TokenSource from an exchanged Kerberos credential
// (a ccache from kerbexchange requesting OutputCCache) targeting spn. Each call
// builds a new SPNEGO NegTokenInit locally, with a fresh authenticator
// (timestamp, subsecond counter, sequence number), from the ticket cached in the
// ccache: holder-of-key, no KDC round trip. Using one source for many requests is
// what makes a replay-cache-enforcing acceptor (MIT krb5, mod_auth_gssapi) accept
// all of them.
//
// The service is spn.Service + "/" + spn.Host, REALM-LESS: the ccache ticket
// lookup keys on the realm-less principal string, and a realm-ful name would miss
// the cache and attempt a live TGS exchange, which has no KDC to talk to.
func KerberosTokenSource(cred *kerb.Credential, spn kerb.ServicePrincipal) (TokenSource, error) {
	b, err := cred.CCache()
	if err != nil {
		return nil, fmt.Errorf("negotiate: ccache: %w", err)
	}
	var cc credentials.CCache
	if err := cc.Unmarshal(b); err != nil {
		return nil, fmt.Errorf("negotiate: parse ccache: %w", err)
	}
	kc, err := saslgssapi.FromCCache(&cc)
	if err != nil {
		return nil, fmt.Errorf("negotiate: client from ccache: %w", err)
	}
	return newKerberosSource(kc, spn.Service+"/"+spn.Host), nil
}

func newKerberosSource(kc *client.Client, service string) TokenSource {
	// The krb5 client is not documented as safe for concurrent ticket lookups, and
	// the DAV client may issue requests concurrently; serialize minting.
	var mu sync.Mutex
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		s := spnego.SPNEGOClient(kc, service)
		// No AcquireCred: it would AffirmLogin, which wants a TGT session the
		// holder-of-key ccache (service ticket only) does not have. InitSecContext
		// serves the cached ticket without a KDC.
		st, err := s.InitSecContext()
		if err != nil {
			return "", fmt.Errorf("init security context: %w", err)
		}
		raw, err := st.Marshal()
		if err != nil {
			return "", fmt.Errorf("marshal SPNEGO token: %w", err)
		}
		return base64.StdEncoding.EncodeToString(raw), nil
	}
}
