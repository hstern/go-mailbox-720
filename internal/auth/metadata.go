package auth

import (
	"net/http"
	"time"

	prm "github.com/hstern/go-protected-resource-metadata"
)

// ResourceMetadata builds this resource's RFC 9728 protected-resource metadata
// document from cfg: ResourceID is the required "resource" member, Issuers are the
// authorization servers, RequiredScopes are advertised as scopes_supported, and the
// only bearer method is the Authorization header (RFC 6750 §2.1).
func ResourceMetadata(cfg Config) *prm.Metadata {
	return &prm.Metadata{
		Resource:               cfg.ResourceID,
		AuthorizationServers:   cfg.Issuers,
		ScopesSupported:        cfg.RequiredScopes,
		BearerMethodsSupported: []string{prm.BearerMethodHeader},
	}
}

// MetadataEndpoint returns the request path and handler that publish this resource's
// RFC 9728 metadata document. It is PUBLIC — clients fetch it unauthenticated to
// discover the authorization servers and scopes — so mount it OUTSIDE the auth
// middleware. The path follows the §3.1 well-known construction for cfg.ResourceID;
// an invalid resource identifier is reported as an error.
func MetadataEndpoint(cfg Config) (path string, h http.Handler, err error) {
	m := ResourceMetadata(cfg)
	path, err = prm.WellKnownRequestPath(m.Resource)
	if err != nil {
		return "", nil, err
	}
	return path, m.Handler(prm.WithMaxAge(time.Hour)), nil
}
