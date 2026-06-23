// zitadel.go is the Zitadel implementation of the idp interface. Unlike Kanidm,
// Zitadel issues RFC-9068-shaped JWT access tokens (validated by mailboxd via JWKS,
// no introspection) over plain HTTP, and it can perform real RFC 8693 user
// impersonation — so zitadelIDP reports impersonation=true. It runs as two
// containers (Zitadel + PostgreSQL) on a private docker network.
//
// Baseline token: a "mailbox" machine user whose client_credentials JWT carries
// aud=[mailbox] and a subject (Zitadel, unlike SimpleIdServer, includes sub on
// client_credentials and never serialises scope as a JSON array). Impersonation is
// enabled in provision() (security policy + the token-exchange feature is on by
// default) so the capability-gated tier (MB720-41) can exercise it.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	zitadelImage = "ghcr.io/zitadel/zitadel:latest"
	zitadelPG    = "mailbox-e2e-zit-pg"
	zitadelCtr   = "mailbox-e2e-zit"
	zitadelNet   = "mailbox-e2e-zit-net"
	zitadelKey   = "MasterkeyNeedsToHave32Characters" // 32 chars, dev only
	// Zitadel derives its issuer from EXTERNALDOMAIN:EXTERNALPORT, so the host port
	// is fixed to match. It listens on 8080 inside the container.
	zitadelPort = "8085"
	zitadelBase = "http://localhost:" + zitadelPort

	zClientID = "mailbox" // machine-user username == client_id; also the token aud
)

// zitadelIDP implements idp.
type zitadelIDP struct {
	pat      string
	clientID string
	secret   string

	// Impersonation provisioning (provisionImpersonation populates these).
	//
	// exchangeApp{ID,Secret} are an OIDC web app with the token-exchange grant —
	// the exchanging client. Zitadel rejects a machine user as a token-exchange
	// client ("invalid_client: no active client not found"); only an OIDC app with
	// OIDC_GRANT_TYPE_TOKEN_EXCHANGE may perform the grant. So unlike the
	// production peridentity.go (which authenticates as the mailbox machine user),
	// the e2e authenticates the exchange as this app. The sub-preserving exchange
	// property is identical; only the client registration differs.
	exchangeApp    string
	exchangeAppSec string

	// backendAud maps each logical backend audience (the aud* constants) to the
	// Zitadel project id it was registered as. The exchanged token's aud carries
	// these numeric project ids (Zitadel only admits project ids — resolved at
	// provisioning time — into a token's aud, never arbitrary strings).
	backendAud map[string]string

	// users maps a username (userA/userB) to its machine-user client credentials.
	users map[string]clientCreds
}

// clientCreds is a machine user's client_credentials login.
type clientCreds struct{ clientID, secret string }

func (z *zitadelIDP) name() string               { return "zitadel" }
func (z *zitadelIDP) issuer() string             { return zitadelBase }
func (z *zitadelIDP) audience() string           { return z.clientID }
func (z *zitadelIDP) scope() string              { return "" } // client_credentials carries no scope claim
func (z *zitadelIDP) introspectClientID() string { return "" } // JWT validated via JWKS
func (z *zitadelIDP) introspectSecret() string   { return "" }
func (z *zitadelIDP) sslCertFile() string        { return "" } // plain HTTP
func (z *zitadelIDP) caps() idpCaps              { return idpCaps{impersonation: true} }

func (z *zitadelIDP) start(t *testing.T) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", zitadelCtr, zitadelPG).Run()
	_ = exec.Command("docker", "network", "rm", zitadelNet).Run()
	run(t, "docker", "network", "create", zitadelNet)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", zitadelCtr, zitadelPG).Run()
		_ = exec.Command("docker", "network", "rm", zitadelNet).Run()
	})

	run(t, "docker", "run", "-d", "--name", zitadelPG, "--network", zitadelNet,
		"-e", "POSTGRES_USER=zitadel", "-e", "POSTGRES_PASSWORD=zitadel", "-e", "POSTGRES_DB=zitadel",
		"postgres:16-alpine")
	waitFor(t, "zitadel-postgres", 60*time.Second, func() bool {
		return exec.Command("docker", "exec", zitadelPG, "pg_isready", "-U", "zitadel", "-d", "zitadel").Run() == nil
	})

	patDir := t.TempDir()
	if err := os.Chmod(patDir, 0o777); err != nil { // writable by the container's uid
		t.Fatal(err)
	}
	run(t, "docker", "run", "-d", "--name", zitadelCtr, "--network", zitadelNet,
		"-p", zitadelPort+":8080",
		"-e", "ZITADEL_EXTERNALSECURE=false",
		"-e", "ZITADEL_EXTERNALDOMAIN=localhost",
		"-e", "ZITADEL_EXTERNALPORT="+zitadelPort,
		"-e", "ZITADEL_DATABASE_POSTGRES_HOST="+zitadelPG,
		"-e", "ZITADEL_DATABASE_POSTGRES_PORT=5432",
		"-e", "ZITADEL_DATABASE_POSTGRES_DATABASE=zitadel",
		"-e", "ZITADEL_DATABASE_POSTGRES_USER_USERNAME=zitadel",
		"-e", "ZITADEL_DATABASE_POSTGRES_USER_PASSWORD=zitadel",
		"-e", "ZITADEL_DATABASE_POSTGRES_USER_SSL_MODE=disable",
		"-e", "ZITADEL_DATABASE_POSTGRES_ADMIN_USERNAME=zitadel",
		"-e", "ZITADEL_DATABASE_POSTGRES_ADMIN_PASSWORD=zitadel",
		"-e", "ZITADEL_DATABASE_POSTGRES_ADMIN_SSL_MODE=disable",
		"-e", "ZITADEL_FIRSTINSTANCE_ORG_MACHINE_MACHINE_USERNAME=zadmin",
		"-e", "ZITADEL_FIRSTINSTANCE_ORG_MACHINE_MACHINE_NAME=zadmin",
		"-e", "ZITADEL_FIRSTINSTANCE_ORG_MACHINE_PAT_EXPIRATIONDATE=2030-01-01T00:00:00Z",
		"-e", "ZITADEL_FIRSTINSTANCE_PATPATH=/pat/pat.txt",
		"-v", patDir+":/pat",
		zitadelImage, "start-from-init", "--masterkey", zitadelKey, "--tlsMode", "disabled")

	// init+setup+start with migrations is slow; the OIDC endpoint also comes up
	// before the gRPC admin/management backend, so gate on discovery first…
	waitFor(t, "zitadel", 180*time.Second, func() bool {
		resp, err := http.Get(zitadelBase + "/.well-known/openid-configuration")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	z.pat = z.readPAT(t, patDir+"/pat.txt")
	// …then on an authenticated admin call, which fails with 503 until the gRPC
	// backend is reachable.
	waitFor(t, "zitadel-admin", 120*time.Second, func() bool {
		req, err := http.NewRequest(http.MethodGet, zitadelBase+"/admin/v1/policies/security", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+z.pat)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

func (z *zitadelIDP) provision(t *testing.T) {
	t.Helper()
	// Enable end-user impersonation (the oidc_token_exchange feature is on by
	// default) so the capability-gated impersonation tier can exercise it.
	z.mgmt(t, http.MethodPut, "/admin/v1/policies/security", map[string]any{"enableImpersonation": true})

	// The mailbox resource server: a machine user whose JWT client_credentials token
	// carries aud=[username] and a subject. accessTokenType JWT so mailboxd validates
	// via JWKS.
	var mu struct {
		UserID string `json:"userId"`
	}
	z.mgmtJSON(t, http.MethodPost, "/management/v1/users/machine", map[string]any{
		"userName": zClientID, "name": "Mailbox API", "accessTokenType": "ACCESS_TOKEN_TYPE_JWT",
	}, &mu)

	var sec struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	z.mgmtJSON(t, http.MethodPut, "/management/v1/users/"+mu.UserID+"/secret", map[string]any{}, &sec)
	z.clientID, z.secret = sec.ClientID, sec.ClientSecret
}

func (z *zitadelIDP) mintToken(t *testing.T) string {
	t.Helper()
	return postToken(t, http.DefaultClient, zitadelBase+"/oauth/v2/token",
		url.Values{"grant_type": {"client_credentials"}, "scope": {"openid"}},
		z.clientID, z.secret)
}

func (z *zitadelIDP) readPAT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read zitadel PAT: %v", err)
	}
	pat := string(bytes.TrimSpace(b))
	if pat == "" {
		t.Fatal("zitadel PAT file is empty")
	}
	return pat
}

// mgmt calls the Zitadel admin/management API with the bootstrap PAT, failing on a
// non-2xx response, and returns the body.
func (z *zitadelIDP) mgmt(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, zitadelBase+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+z.pat)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("zitadel %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("zitadel %s %s -> %d: %s", method, path, resp.StatusCode, rb)
	}
	return rb
}

// mgmtJSON is mgmt with the response decoded into out.
func (z *zitadelIDP) mgmtJSON(t *testing.T, method, path string, body, out any) {
	t.Helper()
	rb := z.mgmt(t, method, path, body)
	if err := json.Unmarshal(rb, out); err != nil {
		t.Fatalf("zitadel %s %s decode: %v (%s)", method, path, err, rb)
	}
}

// --- RFC 8693 impersonation provisioning (MB720-52) ---

// backendAudiences are the logical per-protocol backend audiences the e2e
// exchanges for, in a fixed order so provisioning is deterministic.
var backendAudiences = []string{
	audMailJMAP, audContactsJMAP, audCalDAV, audCardDAV, audIMAP, audSMTP,
}

func (z *zitadelIDP) tokenEndpoint() string    { return zitadelBase + "/oauth/v2/token" }
func (z *zitadelIDP) exchangeClientID() string { return z.exchangeApp }
func (z *zitadelIDP) exchangeSecret() string   { return z.exchangeAppSec }

// backendAudience resolves a logical backend audience (an aud* constant) to the
// concrete value the exchanged token will carry — the Zitadel project id it was
// registered as. The exchange's `audience` param and the resulting `aud` claim use
// this value, not the symbolic constant, because Zitadel only admits registered
// project ids into a token's aud (an arbitrary string fails with invalid_target).
func (z *zitadelIDP) backendAudience(t *testing.T, logical string) string {
	t.Helper()
	id, ok := z.backendAud[logical]
	if !ok {
		t.Fatalf("backend audience %q was not provisioned", logical)
	}
	return id
}

// provisionImpersonation makes the RFC 8693 sub-preserving exchange work end to
// end. It (1) registers each backend audience as a Zitadel project so the
// exchange's audience param resolves to a real aud, (2) creates an OIDC web app
// with the token-exchange grant as the exchanging client mailboxd authenticates
// with, and (3) creates userA/userB as machine users whose client_credentials
// tokens carry every backend audience (via the reserved project-audience scope) so
// the exchange may narrow to any one of them.
//
// Note on roles: a bare exchange that keeps the SAME subject (subject_token = a
// user's token, no actor_token) is a standard RFC 8693 exchange — it preserves the
// subject's sub and needs no impersonation role. The IAM_END_USER_IMPERSONATOR
// role + actor_token are only required to CHANGE the subject (act as a user via a
// different actor's token), which this path does not do. enableImpersonation is
// already set in provision(); the role is left for the later actor-token tier.
func (z *zitadelIDP) provisionImpersonation(t *testing.T) {
	t.Helper()

	// 1. One project per backend audience; record logical -> project id.
	z.backendAud = make(map[string]string, len(backendAudiences))
	for _, aud := range backendAudiences {
		var p struct {
			ID string `json:"id"`
		}
		z.mgmtJSON(t, http.MethodPost, "/management/v1/projects", map[string]any{"name": aud}, &p)
		z.backendAud[aud] = p.ID
	}

	// 2. The exchanging client: an OIDC web app with the token-exchange grant.
	//    accessTokenType JWT so the exchanged token is a decodable/JWKS-validatable
	//    JWT (requested_token_type=jwt). A machine user cannot perform this grant.
	pid := z.backendAud[audMailJMAP] // any project hosts the app; reuse the first.
	var app struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	z.mgmtJSON(t, http.MethodPost, "/management/v1/projects/"+pid+"/apps/oidc", map[string]any{
		"name":            "mailbox-exchanger",
		"responseTypes":   []string{"OIDC_RESPONSE_TYPE_CODE"},
		"grantTypes":      []string{"OIDC_GRANT_TYPE_TOKEN_EXCHANGE", "OIDC_GRANT_TYPE_AUTHORIZATION_CODE"},
		"appType":         "OIDC_APP_TYPE_WEB",
		"authMethodType":  "OIDC_AUTH_METHOD_TYPE_BASIC",
		"accessTokenType": "OIDC_TOKEN_TYPE_JWT",
	}, &app)
	z.exchangeApp, z.exchangeAppSec = app.ClientID, app.ClientSecret

	// 3. The subject users.
	z.users = make(map[string]clientCreds, 2)
	for _, u := range []string{userA, userB} {
		z.users[u] = z.createMachineUser(t, u)
	}
}

// createMachineUser creates a machine user with JWT access tokens and returns its
// client_credentials login.
func (z *zitadelIDP) createMachineUser(t *testing.T, username string) clientCreds {
	t.Helper()
	var mu struct {
		UserID string `json:"userId"`
	}
	z.mgmtJSON(t, http.MethodPost, "/management/v1/users/machine", map[string]any{
		"userName": username, "name": username, "accessTokenType": "ACCESS_TOKEN_TYPE_JWT",
	}, &mu)
	var sec struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	z.mgmtJSON(t, http.MethodPut, "/management/v1/users/"+mu.UserID+"/secret", map[string]any{}, &sec)
	return clientCreds{clientID: sec.ClientID, secret: sec.ClientSecret}
}

// mintUserToken returns a subject access token for the named user whose aud carries
// every backend audience (the reserved project-audience scope adds each project id
// to the token), so a later exchange may narrow to any one of them.
func (z *zitadelIDP) mintUserToken(t *testing.T, user string) string {
	t.Helper()
	cc, ok := z.users[user]
	if !ok {
		t.Fatalf("user %q was not provisioned", user)
	}
	scopes := []string{"openid"}
	for _, aud := range backendAudiences {
		scopes = append(scopes, "urn:zitadel:iam:org:project:id:"+z.backendAud[aud]+":aud")
	}
	return postToken(t, http.DefaultClient, z.tokenEndpoint(),
		url.Values{"grant_type": {"client_credentials"}, "scope": {strings.Join(scopes, " ")}},
		cc.clientID, cc.secret)
}
