package davauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
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
// mutual-auth reply is not processed, so server authenticity rests on TLS; DAV
// URLs should be https in production.
//
// When c is an *http.Client the token is minted in its Transport, i.e. once per
// network round trip, so a followed same-host redirect gets its own token rather
// than replaying the first (a replay-cache acceptor would reject that). To keep
// tokens from leaking, Do only sets a placeholder "Authorization: Negotiate"
// header and records the request's origin (scheme and host); net/http's redirect
// logic then decides whether the header survives (it strips it on a cross-domain
// redirect), and the Transport mints a real token only when the placeholder is
// still present AND the round trip targets the original origin. A redirect to
// another host, or a scheme downgrade such as https to http, therefore carries no
// Authorization header and no token is minted for it. Redirects are matched on the literal scheme and
// host, so an http to https redirect (or one differing in default-port or host-case spelling) deliberately
// loses auth: configure the canonical https URL. Any other
// webdav.HTTPClient is wrapped at Do, minting once per call; its own redirect
// handling is outside this package's control.
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
		return &negotiateHTTPClient{c: &cp, src: src, perTrip: true}
	}
	return &negotiateHTTPClient{c: c, src: src}
}

// placeholderAuth marks a request whose Authorization the Transport should fill
// with a fresh token. It is never sent to a server.
const placeholderAuth = "Negotiate"

type originKey struct{}

func originOf(u *url.URL) string { return u.Scheme + "://" + u.Host }

type negotiateTransport struct {
	rt  http.RoundTripper
	src TokenSource
}

func (n *negotiateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != placeholderAuth {
		return n.rt.RoundTrip(req)
	}
	origin, _ := req.Context().Value(originKey{}).(string)
	out := req.Clone(req.Context()) // RoundTrip must not mutate its argument
	if origin == originOf(req.URL) {
		tok, err := n.src()
		if err != nil {
			return nil, fmt.Errorf("negotiate: mint token: %w", err)
		}
		out.Header.Set("Authorization", "Negotiate "+tok)
	} else {
		out.Header.Del("Authorization")
	}
	resp, err := n.rt.RoundTrip(out)
	if resp != nil {
		// The transport points resp.Request at the clone carrying the real token;
		// restore the placeholder request so logging or tracing resp.Request does
		// not expose the AP-REQ.
		resp.Request = req
	}
	return resp, err
}

type negotiateHTTPClient struct {
	c       webdav.HTTPClient
	src     TokenSource
	perTrip bool // the Transport mints; Do only sets the placeholder
}

func (n *negotiateHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if n.perTrip {
		req = req.Clone(context.WithValue(req.Context(), originKey{}, originOf(req.URL)))
		req.Header.Set("Authorization", placeholderAuth)
		return n.c.Do(req)
	}
	tok, err := n.src()
	if err != nil {
		return nil, fmt.Errorf("negotiate: mint token: %w", err)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Negotiate "+tok)
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
