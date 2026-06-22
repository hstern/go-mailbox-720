// Package davauth adds OAuth 2.0 Bearer authentication to a go-webdav HTTPClient
// — the WebDAV analogue of webdav.HTTPClientWithBasicAuth, which go-webdav ships
// only for Basic. The CalDAV and CardDAV adapters use it for the per-identity
// path (MB720-44): each request to the backend carries the user's exchanged
// backend-audience token (RFC 8693) as Authorization: Bearer.
package davauth

import (
	"net/http"

	"github.com/emersion/go-webdav"
)

// BearerHTTPClient wraps c so every request it performs carries an
// "Authorization: Bearer <token>" header. A nil c defaults to http.DefaultClient.
// It mirrors webdav.HTTPClientWithBasicAuth, which likewise sets the auth header
// in place on each request the go-webdav client issues.
func BearerHTTPClient(c webdav.HTTPClient, token string) webdav.HTTPClient {
	if c == nil {
		c = http.DefaultClient
	}
	return &bearerHTTPClient{c: c, token: token}
}

type bearerHTTPClient struct {
	c     webdav.HTTPClient
	token string
}

func (b *bearerHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.c.Do(req)
}
