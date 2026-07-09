package davauth

import (
	"net/http"

	"github.com/emersion/go-webdav"
)

// NegotiateHTTPClient wraps c so every request it performs carries an
// "Authorization: Negotiate <token>" header (RFC 4559). A nil c defaults to
// http.DefaultClient. It mirrors BearerHTTPClient for the second per-identity
// mechanism (MB720-47): each request to a SPNEGO/Kerberos-fronted DAV backend
// carries a GSSAPI credential minted for the authenticated user.
//
// token is the base64 encoding of a GSSAPI initial-context token (a KRB_AP_REQ
// wrapped per RFC 2743 §3.1) — the OutputAPReq shape from
// go-oauth2-kerberos-exchange, which is meant to be dropped verbatim into one
// HTTP Negotiate exchange. It is presented bearer-style: mailboxd sends only the
// initial token and does not process the server's mutual-auth reply. The same
// token rides every request the go-webdav client issues on the cached backend, so
// a backend that enforces a Kerberos replay cache across requests would reject the
// reuse; that is a deployment/server-policy concern, not handled here.
func NegotiateHTTPClient(c webdav.HTTPClient, token string) webdav.HTTPClient {
	if c == nil {
		c = http.DefaultClient
	}
	return &negotiateHTTPClient{c: c, token: token}
}

type negotiateHTTPClient struct {
	c     webdav.HTTPClient
	token string
}

func (n *negotiateHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Negotiate "+n.token)
	return n.c.Do(req)
}
