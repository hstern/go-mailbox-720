# MB720-52 — Impersonation→Backend E2E Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** End-to-end test the per-identity backend path — a real IdP (Zitadel) issues a user token, mailboxd validates and RFC 8693-exchanges it (sub-preserving) for a backend-audience token, and a lightweight in-process backend that validates that exchanged token serves the user's own data — across all six per-identity paths, proving no cross-tenant bleed.

**Architecture:** All new code is test-only, in the existing `test/e2e-oidc` Go module. A shared `tokenValidator` (verifies a JWT against Zitadel's JWKS: iss + aud + sig + exp → returns sub) and a shared per-`sub` `userStore` back four small in-process fake servers (JMAP/HTTP, WebDAV, IMAP, SMTP), each authenticating with the validator and serving from the store. Zitadel provisioning is extended to grant the `mailbox` client the impersonator role + token-exchange grant, register backend audiences, and create two subject users. One top-level `Test...` per protocol drives mailboxd (subprocess) with `-tokenexchange-endpoint` + the protocol's `-*-audience` flag.

**Tech Stack:** Go; `test/e2e-oidc` module (already requires `emersion/go-imap/v2`, `emersion/go-smtp`, `emersion/go-sasl`, and the parent module's `emersion/go-webdav`); Docker (Zitadel + Postgres) via `os/exec`; Zitadel admin/management REST API.

## Global Constraints

- All code lives under `test/e2e-oidc/` (package `e2e`); no changes to mailboxd production code — the per-identity providers already exist and are wired.
- Impersonation is Zitadel-only: every new test gates on the Zitadel IdP (`zitadelIDP.caps().impersonation == true`); Kanidm is excluded.
- Tests self-skip when Docker is unavailable (reuse the existing `dockerAvailable`/skip guard at the top of `TestOIDCEndToEnd`).
- Before any `go test` in the e2e module, the parent module must have run `go generate ./internal/graph` (generated Graph types). Run it once at the repo root.
- Secrets (client secret, tokens) pass via env to the mailboxd subprocess, never via flags — mirror `startMailboxd`'s existing `MAILBOXD_*` env pattern. The token-exchange client secret env var is `MAILBOXD_TOKENEXCHANGE_CLIENT_SECRET`.
- Reuse existing helpers in the module: `run`, `waitFor`, `freeAddr`, `buildMailboxd`, `get`, `status`, `doJSON`, `postToken`, `authFlags`, `authEnv`. Do not duplicate them.
- mailboxd client-facing base URL is `http://<addr>/v1.0`; routes are `/me/messages`, `/me/events`, `/me/contacts`; responses are Graph JSON `{ "value": [ ... ] }`.

---

## File Structure

- `test/e2e-oidc/impersonation_test.go` — shared impersonation harness: `startMailboxdImpersonation(...)`, the per-protocol audience constants, and the two-user seed/mint orchestration helpers. Also home of `TestImpersonationExchangeSpike` (Task 0).
- `test/e2e-oidc/tokenvalidator_test.go` — `tokenValidator` (JWKS fetch + verify) and its unit test.
- `test/e2e-oidc/userstore_test.go` — `userStore` (per-sub fixtures + recorders) and its unit test.
- `test/e2e-oidc/jmapfake_test.go` — in-process JMAP HTTP fake (mail + contacts), built on the validator + store, adapted from `internal/mail/jmap/jmap_test.go` and `internal/contacts/jmap/jmap_test.go`.
- `test/e2e-oidc/davfake_test.go` — in-process WebDAV fake (CalDAV + CardDAV), Bearer-authed, adapted from `internal/calendar/caldav` / `internal/contacts/carddav` tests + `internal/davauth/bearer.go`.
- `test/e2e-oidc/imapfake_test.go` — in-process IMAP fake (go-imap/v2 `imapserver`, SASL OAUTHBEARER).
- `test/e2e-oidc/smtpfake_test.go` — in-process SMTP fake (go-smtp server, SASL OAUTHBEARER), extending the `smtpsink` pattern.
- `test/e2e-oidc/zitadel_test.go` — MODIFY: add impersonator role + token-exchange grant + backend-audience registration + two subject users + `mintUserToken`.
- `test/e2e-oidc/idp_test.go` — MODIFY (only if needed): expose impersonation provisioning via a small optional interface so the harness can request it from the Zitadel IdP without polluting the base `idp` interface.
- `.github/workflows/ci.yml` — MODIFY: one job per protocol test.

Per-test files (each its own top-level test, sharing the harness):
- `TestImpersonationJMAPMail` and `TestImpersonationContactsJMAP` → `impersonation_jmap_test.go`
- `TestImpersonationCalDAV` and `TestImpersonationCardDAV` → `impersonation_dav_test.go`
- `TestImpersonationIMAP` → `impersonation_imap_test.go`
- `TestImpersonationSMTP` → `impersonation_smtp_test.go`

---

## Task 0: Zitadel exchange spike — prove the recipe

Isolates the dominant risk (Zitadel impersonation provisioning) before any fake exists. Deliverable: a passing test that mints userA's token, exchanges it for a backend-audience token, and asserts the result preserves userA's `sub` and carries the backend `aud`. The provisioning code it lands becomes the foundation all later tasks reuse.

**Files:**
- Modify: `test/e2e-oidc/zitadel_test.go`
- Modify: `test/e2e-oidc/idp_test.go` (add the optional impersonation interface)
- Create: `test/e2e-oidc/impersonation_test.go` (spike test + shared helpers)

**Interfaces:**
- Consumes: `zitadelIDP.start`, `zitadelIDP.provision`, `zitadelIDP.mgmt`/`mgmtJSON`, `postToken`.
- Produces:
  - `const audMailJMAP, audContactsJMAP, audCalDAV, audCardDAV, audIMAP, audSMTP string` — the per-protocol backend audiences (Zitadel client_ids or project resource ids registered in provisioning). If per-audience registration proves costly, set them all to one shared `audBackend` and note it inline.
  - On `zitadelIDP`: `provisionImpersonation(t)` (grants `IAM_END_USER_IMPERSONATOR` to the `mailbox` user + ensures the token-exchange grant + registers the backend audiences + creates `userA`/`userB`), `mintUserToken(t, user string) string`, and `exchangeClientID()/exchangeSecret()` returning the `mailbox` client credentials mailboxd will authenticate the exchange with.
  - `type impersonator interface { provisionImpersonation(*testing.T); mintUserToken(*testing.T, string) string; exchangeClientID() string; exchangeSecret() string; tokenEndpoint() string }` in `idp_test.go`.
  - `userA = "userא"`-style stable usernames: use `userA = "usera"`, `userB = "userb"`.

- [ ] **Step 1: Write the failing spike test**

In `test/e2e-oidc/impersonation_test.go`:

```go
// impersonation_test.go drives the per-identity backend path end to end:
// Zitadel issues a user token, mailboxd validates + RFC 8693-exchanges it
// (sub-preserving) for a backend-audience token, and an in-process backend that
// validates that exchanged token serves the user's own data (MB720-52).
// Impersonation is Zitadel-only (Kanidm token exchange is service-account only).
package e2e

import (
	"net/url"
	"strings"
	"testing"
)

// Backend audiences requested per protocol. Distinct per protocol so each
// provider's request is exercised; provisionImpersonation registers each.
const (
	audMailJMAP     = "backend-mail-jmap"
	audContactsJMAP = "backend-contacts-jmap"
	audCalDAV       = "backend-caldav"
	audCardDAV      = "backend-carddav"
	audIMAP         = "backend-imap"
	audSMTP         = "backend-smtp"
)

const (
	userA = "usera"
	userB = "userb"
)

func TestImpersonationExchangeSpike(t *testing.T) {
	requireDocker(t) // reuse the existing docker guard helper
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	userTok := z.mintUserToken(t, userA)

	// Exchange userA's token for a backend-audience token, authenticating as the
	// mailbox client — exactly what mailboxd's exchanger does.
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {userTok},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"audience":           {audMailJMAP},
		"scope":              {"openid"},
	}
	exchanged := postToken(t, httpClientFor(z), z.tokenEndpoint(), form, z.exchangeClientID(), z.exchangeSecret())

	subUser := subjectOf(t, userTok)
	subExch := subjectOf(t, exchanged)
	if subExch != subUser {
		t.Fatalf("exchange did not preserve sub: user=%q exchanged=%q", subUser, subExch)
	}
	if !audContains(t, exchanged, audMailJMAP) {
		t.Fatalf("exchanged token missing backend audience %q", audMailJMAP)
	}
	_ = strings.TrimSpace
}
```

Add small JWT-claim helpers in the same file (decode the unverified payload — fine for asserting claims in a test):

```go
// subjectOf returns the "sub" claim of a JWT (payload decoded without
// signature verification — adequate for asserting test expectations).
func subjectOf(t *testing.T, jwt string) string { return claimString(t, jwt, "sub") }

// audContains reports whether the JWT's "aud" (string or []string) contains want.
func audContains(t *testing.T, jwt, want string) bool { /* decode payload, handle aud as string or array */ }
```

(Implement `claimString`/`audContains`/`requireDocker`/`httpClientFor` minimally; `requireDocker` mirrors the skip guard already used by `TestOIDCEndToEnd`, and `httpClientFor(z)` returns `http.DefaultClient` for Zitadel since it is plain HTTP.)

- [ ] **Step 2: Run it; expect a compile failure then a real failure**

Run: `cd test/e2e-oidc && go test -run TestImpersonationExchangeSpike -timeout 600s ./...`
Expected: first FAILS to compile (`provisionImpersonation`, `mintUserToken`, `tokenEndpoint`, etc. undefined). This is the red state — proceed to implement.

- [ ] **Step 3: Implement the Zitadel impersonation provisioning**

In `zitadel_test.go`, add (iterate the exact admin-API shapes against the live container — the building blocks: `enableImpersonation` is already set; the `mailbox` user needs the instance role `IAM_END_USER_IMPERSONATOR` and the OIDC token-exchange grant; backend audiences must be registered entities so the exchanged `aud` resolves):

```go
func (z *zitadelIDP) tokenEndpoint() string  { return zitadelBase + "/oauth/v2/token" }
func (z *zitadelIDP) exchangeClientID() string { return z.clientID }
func (z *zitadelIDP) exchangeSecret() string   { return z.secret }

// provisionImpersonation grants the mailbox client end-user impersonation, makes
// the per-protocol backend audiences resolvable, and creates the two subject
// users whose tokens mailboxd accepts (aud = the mailbox audience).
func (z *zitadelIDP) provisionImpersonation(t *testing.T) {
	t.Helper()
	// 1. Grant the mailbox machine user the IAM_END_USER_IMPERSONATOR instance role.
	//    POST /admin/v1/members  (or /management/v1/...) with userId = the mailbox
	//    user id and roles ["IAM_END_USER_IMPERSONATOR"].
	// 2. Ensure the mailbox OIDC app permits the token-exchange grant (create/patch
	//    the app's grantTypes to include OIDC_GRANT_TYPE_TOKEN_EXCHANGE if needed).
	// 3. Register each backend audience (audMailJMAP, …) as a resolvable entity so
	//    the exchange's `audience` param maps to a real aud. Simplest: one project
	//    per audience (or one project + reserved-scope audience). Record the value
	//    each fake will check.
	// 4. Create userA and userB as machine users with JWT access tokens, granted to
	//    the mailbox project so their client_credentials tokens carry aud = the
	//    mailbox audience that mailboxd validates. Store their client_id/secret.
}

// mintUserToken returns a user (subject) access token for the named user, with
// aud = the mailbox audience so mailboxd's middleware accepts it.
func (z *zitadelIDP) mintUserToken(t *testing.T, user string) string {
	t.Helper()
	// client_credentials for the named user's machine credentials, scope openid,
	// plus the reserved scope that adds the mailbox project audience if required.
}
```

Add the `impersonator` interface to `idp_test.go`:

```go
// impersonator is the optional capability the impersonation e2e needs from an IdP.
type impersonator interface {
	provisionImpersonation(t *testing.T)
	mintUserToken(t *testing.T, user string) string
	tokenEndpoint() string
	exchangeClientID() string
	exchangeSecret() string
}
```

- [ ] **Step 4: Run it to green**

Run: `cd test/e2e-oidc && go test -run TestImpersonationExchangeSpike -timeout 600s ./...`
Expected: PASS — exchanged token's `sub` equals userA's `sub`, and `aud` contains `audMailJMAP`.
If audience registration per protocol is impractical, collapse the six `aud*` constants to one shared `audBackend`, re-run, and add a one-line comment recording the fallback.

- [ ] **Step 5: Commit**

```bash
git add test/e2e-oidc/impersonation_test.go test/e2e-oidc/zitadel_test.go test/e2e-oidc/idp_test.go
git commit -m "Prove Zitadel RFC 8693 impersonation recipe in the e2e harness (MB720-52)"
```

---

## Task 1: Shared `tokenValidator` (RFC 7662 introspection)

> **Design note (from the Task 0 investigation):** production mailboxd hardcodes
> `requested_token_type=access_token` (`internal/tokenexchange/tokenexchange.go:148`),
> and Zitadel returns an **encrypted JWE** for that — opaque to JWKS. It is
> introspectable. So the backend validator MUST use RFC 7662 introspection, not
> JWKS. This matches the existing project memory `kanidm-opaque-tokens-need-introspection`.

**Files:**
- Create: `test/e2e-oidc/tokenvalidator_test.go`
- Modify: `test/e2e-oidc/zitadel_test.go` (add the introspection API-app credential + accessors + a production-shape exchange helper for tests)

**Interfaces:**
- Consumes: Zitadel introspection endpoint + an introspection client credential; `z.backendAudience(t, logical)` (the resolver from Task 0 — backend audiences are runtime Zitadel project ids, not the literal `aud*` strings); the production-shape exchange.
- Produces:
  - On `zitadelIDP`: `introspectionEndpoint() string` (`issuer + "/oauth/v2/introspect"`), `backendIntrospectClientID() string` / `backendIntrospectSecret() string` (an API app created in `provisionImpersonation` whose Basic creds authorize introspection), and `exchangeForBackend(t *testing.T, userToken, backendAud string) string` — performs the **production-shape** exchange (`requested_token_type=access_token`, authenticated as the OIDC exchange app) and returns the issued (opaque) backend token, so validator/fake tests can produce a realistic exchanged token without running mailboxd.
  - `type tokenValidator struct { ... }`
  - `func newTokenValidator(t *testing.T, z *zitadelIDP, backendAud string) *tokenValidator` — captures the introspection endpoint + creds + the required backend audience (a resolved project id).
  - `func (v *tokenValidator) validate(bearer string) (sub string, err error)` — POSTs `token=<bearer>` to the introspection endpoint with HTTP Basic (introspection client creds), requires `active == true` AND that the response carries the required backend audience (Zitadel surfaces it as the reserved scope `urn:zitadel:iam:org:project:id:<projectID>:aud` in `scope`, and/or in `aud`), and returns `sub`. Returns a non-nil error for an inactive token or one lacking the required backend audience.

- [ ] **Step 1: Write the failing test**

```go
package e2e

import "testing"

func TestTokenValidatorChecksBackendAudience(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	// A token exchanged for the CalDAV backend audience must validate for a CalDAV
	// validator (returning the user's sub) and be REJECTED by a JMAP-mail validator
	// — this audience check is what makes the backend "trust the exchanged token"
	// for ITS protocol rather than any Zitadel token.
	userTok := z.mintUserToken(t, userA)
	calTok := z.exchangeForBackend(t, userTok, z.backendAudience(t, audCalDAV))

	calV := newTokenValidator(t, z, z.backendAudience(t, audCalDAV))
	sub, err := calV.validate(calTok)
	if err != nil {
		t.Fatalf("CalDAV validator rejected a CalDAV-audience token: %v", err)
	}
	if want := z.subjectFor(t, userA); sub != want {
		t.Fatalf("validated sub = %q, want %q", sub, want)
	}

	mailV := newTokenValidator(t, z, z.backendAudience(t, audMailJMAP))
	if _, err := mailV.validate(calTok); err == nil {
		t.Fatal("JMAP-mail validator accepted a CalDAV-audience token")
	}
}
```

- [ ] **Step 2: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestTokenValidatorChecksBackendAudience -timeout 600s ./...`
Expected: FAIL — `newTokenValidator`/`tokenValidator.validate`/`exchangeForBackend`/`introspectionEndpoint`/`backendIntrospect*` undefined.

- [ ] **Step 3: Implement the introspection validator + provisioning**

In `zitadel_test.go`: in `provisionImpersonation`, create an API app (e.g. project app with `authMethodType` client_secret_basic, or a machine user) whose Basic creds authorize calls to `POST {issuer}/oauth/v2/introspect`; store its id/secret and expose `backendIntrospectClientID()/backendIntrospectSecret()/introspectionEndpoint()`. Add `exchangeForBackend` (copy the spike's exchange call but with `requested_token_type=access_token` — the production value). In `tokenvalidator_test.go`: `newTokenValidator` stores endpoint+creds+required aud; `validate` does the introspection POST (form `token=<bearer>`, Basic auth), JSON-decodes `{active, sub, aud, scope}`, requires `active` and that the required backend project-id audience appears (in `aud` or as the `urn:zitadel:iam:org:project:id:<pid>:aud` scope token), and returns `sub`. Use only stdlib `net/http`/`encoding/json` — no JWT library needed (the token is opaque).

- [ ] **Step 4: Run to green**

Run: `cd test/e2e-oidc && go test -run TestTokenValidatorChecksBackendAudience -timeout 600s ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/e2e-oidc/tokenvalidator_test.go test/e2e-oidc/zitadel_test.go
git commit -m "Add RFC 7662 introspection validator for impersonation e2e backends (MB720-52)"
```

---

## Task 2: Shared `userStore` + impersonation mailboxd harness

**Files:**
- Create: `test/e2e-oidc/userstore_test.go`
- Modify: `test/e2e-oidc/impersonation_test.go` (add `startMailboxdImpersonation`)
- Modify: `test/e2e-oidc/zitadel_test.go` (mailbox resource project + `mailboxAudience()` + extend `mintUserToken` to carry it — see Concern #2 below)

**Interfaces:**
- Produces:
  - `type message struct { Subject, FromAddr string }`, `type event struct { Subject string }`, `type contact struct { DisplayName string }`, `type sentMail struct { From string; To []string; Data string }`.
  - `type userStore struct { ... }` with `func newUserStore() *userStore`, `func (s *userStore) seedMessages(sub string, m ...message)`, `seedEvents`, `seedContacts`, getters `messages(sub) []message` / `events(sub)` / `contacts(sub)`, and `recordSent(sub string, m sentMail)` / `sent(sub) []sentMail` for SMTP. All keyed by validated `sub`; unknown sub returns empty.
  - `func startMailboxdImpersonation(t *testing.T, z *zitadelIDP, extraArgs []string) string` — builds + runs mailboxd with the auth + token-exchange flags + `extraArgs`, waits for readiness, returns the `/v1.0` base URL. Mirrors `startMailboxd`. Flags: `-auth-issuer z.issuer()`, **`-auth-audience z.mailboxAudience()`** (NOT `authFlags(z)`'s `audience()` — see the concern below), `-tokenexchange-endpoint z.tokenEndpoint()`, `-tokenexchange-client-id z.exchangeClientID()`. Env: `MAILBOXD_TOKENEXCHANGE_CLIENT_SECRET=z.exchangeSecret()` (+ `authEnv(z)`).

> **Concern #2 resolution (from Task 0):** user tokens carry the six backend
> project audiences, NOT the `mailbox` machine-user client_id that
> `zitadelIDP.audience()` returns — so plain `authFlags(z)` would make mailboxd
> reject userA/userB at the front door. This task provisions a dedicated
> **mailbox resource project**, includes its reserved-audience scope in
> `mintUserToken` (so user tokens carry it), and exposes
> `mailboxAudience() string` (that project id) which `startMailboxdImpersonation`
> passes as `-auth-audience`. So `provisionImpersonation` / `mintUserToken` in
> `zitadel_test.go` are extended here, and this is first exercised end-to-end in
> Task 3 (the harness cannot be fully validated without a backend).

- [ ] **Step 1: Write the failing test**

```go
package e2e

import "testing"

func TestUserStoreIsolatesSubjects(t *testing.T) {
	s := newUserStore()
	s.seedMessages("subA", message{Subject: "A-only", FromAddr: "a@example.com"})
	s.seedMessages("subB", message{Subject: "B-only", FromAddr: "b@example.com"})
	if got := s.messages("subA"); len(got) != 1 || got[0].Subject != "A-only" {
		t.Fatalf("subA messages = %+v", got)
	}
	if got := s.messages("subB"); len(got) != 1 || got[0].Subject != "B-only" {
		t.Fatalf("subB messages = %+v", got)
	}
	if got := s.messages("subC"); len(got) != 0 {
		t.Fatalf("unknown sub returned data: %+v", got)
	}
}
```

- [ ] **Step 2: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestUserStoreIsolatesSubjects ./...`
Expected: FAIL — undefined `userStore`.

- [ ] **Step 3: Implement `userStore` (mutex-guarded maps) and `startMailboxdImpersonation`**

`userStore`: a `sync.Mutex` plus `map[string][]message`, `map[string][]event`, `map[string][]contact`, `map[string][]sentMail`. `startMailboxdImpersonation`: copy `startMailboxd`'s structure (buildMailboxd, freeAddr, exec, cleanup, waitFor) but swap the static backend flags/env for the token-exchange flags above + `extraArgs`. In `zitadel_test.go`, add the mailbox resource project (Concern #2): register a project, grant userA/userB to it, add its `urn:zitadel:iam:org:project:id:<pid>:aud` reserved scope to `mintUserToken`, and expose `mailboxAudience()` returning that project id for `-auth-audience`.

- [ ] **Step 4: Run to green**

Run: `cd test/e2e-oidc && go test -run TestUserStoreIsolatesSubjects ./...`
Expected: PASS (no docker needed — this is a pure unit test).

- [ ] **Step 5: Commit**

```bash
git add test/e2e-oidc/userstore_test.go test/e2e-oidc/impersonation_test.go
git commit -m "Add per-subject store + impersonation mailboxd harness (MB720-52)"
```

---

## Task 3: JMAP fake + `TestImpersonationJMAPMail` (the MVP slice)

**Files:**
- Create: `test/e2e-oidc/jmapfake_test.go`
- Create: `test/e2e-oidc/impersonation_jmap_test.go`

**Interfaces:**
- Consumes: `tokenValidator`, `userStore`, `startMailboxdImpersonation`, `z.mintUserToken`, `get`/`status`.
- Produces:
  - `func startJMAPFake(t *testing.T, mailV, contactsV *tokenValidator, store *userStore) (sessionURL string)` — an `httptest.Server` serving the JMAP session document at `sessionURL` and the API endpoint, authenticating each request's `Authorization: Bearer` via the matching validator (mail vs contacts chosen by the requested audience/path), and answering from `store` keyed by the validated `sub`.

- [ ] **Step 1: Read the JMAP client + existing fake**

Read `internal/mail/jmap/jmap.go` (the methods the client issues: `Mailbox/get`, `Email/query`, `Email/get`) and `internal/mail/jmap/jmap_test.go` (its httptest fake server). The fake here is that server plus: (a) Bearer extraction + `tokenValidator.validate` → `sub`, (b) results drawn from `store.messages(sub)`.

- [ ] **Step 2: Write the failing test**

```go
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestImpersonationJMAPMail(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	store.seedMessages(z.subjectFor(t, userA), message{Subject: "A inbox", FromAddr: "a@example.com"})
	store.seedMessages(z.subjectFor(t, userB), message{Subject: "B inbox", FromAddr: "b@example.com"})

	mailV := newTokenValidator(t, z, z.backendAudience(t, audMailJMAP))
	sessionURL := startJMAPFake(t, mailV, nil, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-mail-jmap-session-url", sessionURL,
		"-mail-jmap-audience", z.backendAudience(t, audMailJMAP),
	})

	// userA sees only A's inbox.
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userA), "A inbox")
	// userB sees only B's inbox — no cross-tenant bleed.
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userB), "B inbox")
}

func assertSingleMessageSubject(t *testing.T, base, token, want string) {
	t.Helper()
	code, body := get(t, base+"/me/messages", token)
	if code != http.StatusOK {
		t.Fatalf("/me/messages: %d %s", code, body)
	}
	var resp struct{ Value []struct{ Subject string `json:"subject"` } `json:"value"` }
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(resp.Value) != 1 || resp.Value[0].Subject != want {
		t.Fatalf("messages = %s, want exactly [%q]", body, want)
	}
}
```

Add `z.subjectFor(t, user)` to `zitadel_test.go` (mint that user's token once and return its `sub` via `subjectOf`) so the store can be keyed by the same `sub` the validator will extract.

- [ ] **Step 3: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestImpersonationJMAPMail -timeout 900s ./...`
Expected: FAIL — `startJMAPFake`/`subjectFor`/`assertSingleMessageSubject` undefined.

- [ ] **Step 4: Implement `startJMAPFake`**

Adapt `internal/mail/jmap/jmap_test.go`'s server: serve the session document (advertise the mail capability + an account + the `apiUrl` pointing back at the same server), and handle the API calls the client makes, returning `store.messages(sub)` mapped to JMAP `Email` objects (`subject`, `from`). Reject requests whose Bearer fails `mailV.validate`. The `contactsV` param is wired in Task 4; pass `nil` here.

- [ ] **Step 5: Run to green**

Run: `cd test/e2e-oidc && go test -run TestImpersonationJMAPMail -timeout 900s ./...`
Expected: PASS — the full chain (Zitadel token → mailboxd validate → exchange → JMAP fake validates exchanged token → per-sub data) works, and the two users are isolated.

- [ ] **Step 6: Commit**

```bash
git add test/e2e-oidc/jmapfake_test.go test/e2e-oidc/impersonation_jmap_test.go test/e2e-oidc/zitadel_test.go
git commit -m "E2E: JMAP mail per-identity impersonation slice (MB720-52)"
```

---

## Task 4: `TestImpersonationContactsJMAP`

**Files:**
- Modify: `test/e2e-oidc/jmapfake_test.go` (add contacts methods + wire `contactsV`)
- Create: `test/e2e-oidc/impersonation_contacts_jmap_test.go`

**Interfaces:**
- Consumes: everything from Task 3; `internal/contacts/jmap/jmap.go` (client methods) + `internal/contacts/jmap/jmap_test.go` (fake) for the contacts surface.
- Produces: `startJMAPFake` now also validates with `contactsV` and serves `store.contacts(sub)` for the contacts capability/account.

- [ ] **Step 1: Write the failing test**

```go
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestImpersonationContactsJMAP(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t); z.provision(t); z.provisionImpersonation(t)

	store := newUserStore()
	store.seedContacts(z.subjectFor(t, userA), contact{DisplayName: "A Contact"})
	store.seedContacts(z.subjectFor(t, userB), contact{DisplayName: "B Contact"})

	contactsV := newTokenValidator(t, z, z.backendAudience(t, audContactsJMAP))
	sessionURL := startJMAPFake(t, nil, contactsV, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-contacts-jmap-session-url", sessionURL,
		"-contacts-jmap-audience", z.backendAudience(t, audContactsJMAP),
	})

	assertSingleContact(t, base, z.mintUserToken(t, userA), "A Contact")
	assertSingleContact(t, base, z.mintUserToken(t, userB), "B Contact")
}

func assertSingleContact(t *testing.T, base, token, want string) {
	t.Helper()
	code, body := get(t, base+"/me/contacts", token)
	if code != http.StatusOK { t.Fatalf("/me/contacts: %d %s", code, body) }
	var resp struct{ Value []struct{ DisplayName string `json:"displayName"` } `json:"value"` }
	if err := json.Unmarshal(body, &resp); err != nil { t.Fatalf("decode: %v (%s)", err, body) }
	if len(resp.Value) != 1 || resp.Value[0].DisplayName != want {
		t.Fatalf("contacts = %s, want exactly [%q]", body, want)
	}
}
```

- [ ] **Step 2: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestImpersonationContactsJMAP -timeout 900s ./...`
Expected: FAIL — the JMAP fake does not yet serve contacts.

- [ ] **Step 3: Implement contacts in the JMAP fake**

Extend `startJMAPFake` to advertise the contacts capability + account and answer the contacts client's method calls from `store.contacts(sub)`, validating with `contactsV`. Mirror `internal/contacts/jmap/jmap_test.go`.

- [ ] **Step 4: Run to green**

Run: `cd test/e2e-oidc && go test -run TestImpersonationContactsJMAP -timeout 900s ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/e2e-oidc/jmapfake_test.go test/e2e-oidc/impersonation_contacts_jmap_test.go
git commit -m "E2E: JMAP contacts per-identity impersonation slice (MB720-52)"
```

---

## Task 5: WebDAV fake (CalDAV) + `TestImpersonationCalDAV`

**Files:**
- Create: `test/e2e-oidc/davfake_test.go`
- Create: `test/e2e-oidc/impersonation_dav_test.go`

**Interfaces:**
- Consumes: `tokenValidator`, `userStore`, the parent module's `emersion/go-webdav/caldav` server package, `internal/davauth/bearer.go` (Bearer extraction pattern), and `internal/calendar/caldav` tests for the server shape.
- Produces: `func startCalDAVFake(t *testing.T, v *tokenValidator, store *userStore) (url string)` — an httptest WebDAV server with a caldav backend that authenticates Bearer via `v` and serves `store.events(sub)` from the principal's default calendar.

- [ ] **Step 1: Write the failing test**

```go
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestImpersonationCalDAV(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t); z.provision(t); z.provisionImpersonation(t)

	store := newUserStore()
	store.seedEvents(z.subjectFor(t, userA), event{Subject: "A meeting"})
	store.seedEvents(z.subjectFor(t, userB), event{Subject: "B meeting"})

	v := newTokenValidator(t, z, z.backendAudience(t, audCalDAV))
	url := startCalDAVFake(t, v, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-cal-caldav-url", url,
		"-cal-caldav-audience", z.backendAudience(t, audCalDAV),
	})

	assertSingleEvent(t, base, z.mintUserToken(t, userA), "A meeting")
	assertSingleEvent(t, base, z.mintUserToken(t, userB), "B meeting")
}

func assertSingleEvent(t *testing.T, base, token, want string) {
	t.Helper()
	code, body := get(t, base+"/me/events", token)
	if code != http.StatusOK { t.Fatalf("/me/events: %d %s", code, body) }
	var resp struct{ Value []struct{ Subject string `json:"subject"` } `json:"value"` }
	if err := json.Unmarshal(body, &resp); err != nil { t.Fatalf("decode: %v (%s)", err, body) }
	if len(resp.Value) != 1 || resp.Value[0].Subject != want {
		t.Fatalf("events = %s, want exactly [%q]", body, want)
	}
}
```

- [ ] **Step 2: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestImpersonationCalDAV -timeout 900s ./...`
Expected: FAIL — `startCalDAVFake` undefined.

- [ ] **Step 3: Implement `startCalDAVFake`**

Wrap a `go-webdav/caldav` server `Handler` with a Bearer-auth middleware that calls `v.validate` to get `sub`, then serves a single default calendar whose events come from `store.events(sub)` (encode each as a minimal VEVENT with `SUMMARY`). Use `internal/calendar/caldav`'s tests + `internal/davauth/bearer.go` as the reference for the PROPFIND/REPORT surface the `caldav` client exercises for `/me/events`.

- [ ] **Step 4: Run to green**

Run: `cd test/e2e-oidc && go test -run TestImpersonationCalDAV -timeout 900s ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/e2e-oidc/davfake_test.go test/e2e-oidc/impersonation_dav_test.go
git commit -m "E2E: CalDAV per-identity impersonation slice (MB720-52)"
```

---

## Task 6: WebDAV fake (CardDAV) + `TestImpersonationCardDAV`

**Files:**
- Modify: `test/e2e-oidc/davfake_test.go` (add `startCardDAVFake`)
- Modify: `test/e2e-oidc/impersonation_dav_test.go` (add the test)

**Interfaces:**
- Produces: `func startCardDAVFake(t *testing.T, v *tokenValidator, store *userStore) (url string)` — like `startCalDAVFake` but a `go-webdav/carddav` backend serving `store.contacts(sub)` as vCards.

- [ ] **Step 1: Write the failing test**

```go
func TestImpersonationCardDAV(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t); z.provision(t); z.provisionImpersonation(t)

	store := newUserStore()
	store.seedContacts(z.subjectFor(t, userA), contact{DisplayName: "A Card"})
	store.seedContacts(z.subjectFor(t, userB), contact{DisplayName: "B Card"})

	v := newTokenValidator(t, z, z.backendAudience(t, audCardDAV))
	url := startCardDAVFake(t, v, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-contacts-carddav-url", url,
		"-contacts-carddav-audience", z.backendAudience(t, audCardDAV),
	})

	assertSingleContact(t, base, z.mintUserToken(t, userA), "A Card")
	assertSingleContact(t, base, z.mintUserToken(t, userB), "B Card")
}
```

- [ ] **Step 2: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestImpersonationCardDAV -timeout 900s ./...`
Expected: FAIL — `startCardDAVFake` undefined.

- [ ] **Step 3: Implement `startCardDAVFake`**

Mirror `startCalDAVFake` with a `go-webdav/carddav` backend; encode each `store.contacts(sub)` entry as a minimal vCard (`FN`). Reference `internal/contacts/carddav` tests for the addressbook-home-set + REPORT surface `/me/contacts` exercises.

- [ ] **Step 4: Run to green**

Run: `cd test/e2e-oidc && go test -run TestImpersonationCardDAV -timeout 900s ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/e2e-oidc/davfake_test.go test/e2e-oidc/impersonation_dav_test.go
git commit -m "E2E: CardDAV per-identity impersonation slice (MB720-52)"
```

---

## Task 7: IMAP fake + `TestImpersonationIMAP`

**Files:**
- Create: `test/e2e-oidc/imapfake_test.go`
- Create: `test/e2e-oidc/impersonation_imap_test.go`

**Interfaces:**
- Consumes: `emersion/go-imap/v2/imapserver`, `emersion/go-sasl`, `tokenValidator`, `userStore`.
- Produces: `func startIMAPFake(t *testing.T, v *tokenValidator, store *userStore) (addr string)` — a TCP `imapserver` (no TLS) whose SASL OAUTHBEARER mechanism validates the token via `v` (the authzid is the subject; require it to match the token's `sub`), exposing an INBOX whose FETCH results come from `store.messages(sub)`.

- [ ] **Step 1: Confirm the OAUTHBEARER server hook**

Read `imapserver` docs/source in the vendored `go-imap/v2` (the module is `replace`d to `hstern/go-imap/v2`): find how a `Session` advertises and handles SASL (the `imapserver.Session`/`SASL` interface). The OAUTHBEARER mechanism is provided by `go-sasl`; the server side calls back to authenticate.

- [ ] **Step 2: Write the failing test**

```go
package e2e

import "testing"

func TestImpersonationIMAP(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t); z.provision(t); z.provisionImpersonation(t)

	store := newUserStore()
	store.seedMessages(z.subjectFor(t, userA), message{Subject: "A imap", FromAddr: "a@example.com"})
	store.seedMessages(z.subjectFor(t, userB), message{Subject: "B imap", FromAddr: "b@example.com"})

	v := newTokenValidator(t, z, z.backendAudience(t, audIMAP))
	addr := startIMAPFake(t, v, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-mail-imap-addr", addr,
		"-mail-imap-tls=false",
		"-mail-imap-audience", z.backendAudience(t, audIMAP),
	})

	assertSingleMessageSubject(t, base, z.mintUserToken(t, userA), "A imap")
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userB), "B imap")
}
```

- [ ] **Step 3: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestImpersonationIMAP -timeout 900s ./...`
Expected: FAIL — `startIMAPFake` undefined.

- [ ] **Step 4: Implement `startIMAPFake`**

Stand up an `imapserver.Server` on `freeAddr(t)` with a `Session` per connection that: handles `AUTHENTICATE OAUTHBEARER` by validating the SASL token via `v.validate` (and checking the SASL `username`/authzid equals the resulting `sub`), then serves SELECT INBOX + FETCH from `store.messages(sub)` (one message per entry, with the seeded `Subject`/`From`). Keep it to exactly the commands the `internal/mail/imap` client issues for `/me/messages` (read `internal/mail/imap/imap.go`).

- [ ] **Step 5: Run to green**

Run: `cd test/e2e-oidc && go test -run TestImpersonationIMAP -timeout 900s ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add test/e2e-oidc/imapfake_test.go test/e2e-oidc/impersonation_imap_test.go
git commit -m "E2E: IMAP OAUTHBEARER per-identity impersonation slice (MB720-52)"
```

---

## Task 8: SMTP fake + `TestImpersonationSMTP`

**Files:**
- Create: `test/e2e-oidc/smtpfake_test.go`
- Create: `test/e2e-oidc/impersonation_smtp_test.go`

**Interfaces:**
- Consumes: `emersion/go-smtp`, `emersion/go-sasl`, `tokenValidator`, `userStore`, the existing `smtpsink` pattern (`smtpsink_test.go`), and the meeting accept/decline flow that triggers SMTP (read `dumb_backend_test.go` lines ~260+ for how a reply email is emitted).
- Produces: `func startSMTPFake(t *testing.T, v *tokenValidator, store *userStore) (addr string)` — a go-smtp server whose SASL OAUTHBEARER auth validates via `v` → `sub`, recording each delivered message via `store.recordSent(sub, …)`.

- [ ] **Step 1: Write the failing test**

The SMTP path is exercised by accepting a meeting invite, which emails an iMIP reply. Drive it the way `dumb_backend_test.go` does (CalDAV backend for the calendar + SMTP for the reply). Seed an invitation event for userA, accept it as userA, and assert the SMTP fake recorded a reply whose sender corresponds to userA's subject — and that userB's acceptance is recorded under userB.

```go
package e2e

import "testing"

func TestImpersonationSMTP(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t); z.provision(t); z.provisionImpersonation(t)

	store := newUserStore()
	calV := newTokenValidator(t, z, z.backendAudience(t, audCalDAV))
	calURL := startCalDAVFake(t, calV, store) // accept-meeting needs a calendar backend
	smtpV := newTokenValidator(t, z, z.backendAudience(t, audSMTP))
	smtpAddr := startSMTPFake(t, smtpV, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-cal-caldav-url", calURL, "-cal-caldav-audience", z.backendAudience(t, audCalDAV),
		"-smtp-addr", smtpAddr, "-smtp-audience", z.backendAudience(t, audSMTP),
	})

	acceptSeededInvite(t, base, z, store, userA) // helper: seed invite for userA, POST accept, return
	if got := store.sent(z.subjectFor(t, userA)); len(got) == 0 {
		t.Fatal("no iMIP reply recorded for userA")
	}
}
```

(`acceptSeededInvite` adapts the organizer/attendee flow in `dumb_backend_test.go`: seed an event with an attendee for the user, then POST the accept/decline that mailboxd turns into an SMTP reply. Keep it minimal — the assertion is "a reply was sent for this subject".)

- [ ] **Step 2: Run; expect failure**

Run: `cd test/e2e-oidc && go test -run TestImpersonationSMTP -timeout 900s ./...`
Expected: FAIL — `startSMTPFake`/`acceptSeededInvite` undefined.

- [ ] **Step 3: Implement `startSMTPFake` + `acceptSeededInvite`**

`startSMTPFake`: a `go-smtp` server (`AllowInsecureAuth = true`, like the sink) whose `Backend`/`Session` implements SASL OAUTHBEARER, validating via `smtpV.validate` to get `sub`, and on `Data` calling `store.recordSent(sub, sentMail{From, To, Data})`. `acceptSeededInvite`: follow `dumb_backend_test.go`'s scheduling flow to make mailboxd emit one reply.

- [ ] **Step 4: Run to green**

Run: `cd test/e2e-oidc && go test -run TestImpersonationSMTP -timeout 900s ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/e2e-oidc/smtpfake_test.go test/e2e-oidc/impersonation_smtp_test.go
git commit -m "E2E: SMTP OAUTHBEARER per-identity impersonation slice (MB720-52)"
```

---

## Task 9: CI — one job per protocol test

**Files:**
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: the existing e2e job pattern (see the `TestOIDCEndToEnd` / `TestDumbBackendEndToEnd` jobs: `go generate ./internal/graph` first, then `cd test/e2e-oidc && go test -timeout ... -run ... ./...`).

- [ ] **Step 1: Read the existing e2e jobs**

Read `.github/workflows/ci.yml` around the `TestOIDCEndToEnd` and `stalwart-e2e` jobs to copy their runner/setup/`go generate` steps exactly.

- [ ] **Step 2: Add one job per test**

For each of `TestImpersonationJMAPMail`, `TestImpersonationContactsJMAP`, `TestImpersonationCalDAV`, `TestImpersonationCardDAV`, `TestImpersonationIMAP`, `TestImpersonationSMTP`, add a job mirroring the existing e2e job, with:

```yaml
      - name: Impersonation e2e (<X>)
        run: |
          go generate ./internal/graph
          cd test/e2e-oidc
          go test -timeout 900s -run <TestName> ./...
```

- [ ] **Step 3: Validate the workflow locally**

Run: `python -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))" && echo OK`
Expected: `OK` (valid YAML).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "CI: run the per-protocol impersonation e2e jobs (MB720-52)"
```

---

## Self-Review

**Spec coverage:**
- Lightweight validating backend → Task 1 (`tokenValidator`) + the four fakes (Tasks 3–8). ✓
- All six per-identity paths → Tasks 3 (mail-JMAP), 4 (contacts-JMAP), 5 (CalDAV), 6 (CardDAV), 7 (IMAP), 8 (SMTP). ✓
- One top-level test per protocol → each task adds its own `Test...`. ✓
- No cross-tenant bleed → every protocol test asserts userA and userB each see only their own seeded data. ✓
- Negative (wrong-audience rejected) → Task 1's validator test asserts the property directly; each fake reuses that validator. ✓
- Zitadel provisioning (impersonator role + token-exchange grant + audiences + two users) → Task 0. ✓
- CI per protocol → Task 9. ✓
- Distinct-audience-with-shared-fallback decision → Task 0 Step 4 records the fallback path. ✓

**Placeholder scan:** The Zitadel admin-API call shapes in Task 0 Step 3 and the fake-server internals (Tasks 3–8) are expressed as "adapt this exact existing file + these exact methods" rather than literal code, because the precise wire shapes must be read from the named source files and (for Zitadel) iterated against the live container. Each names the exact file to mirror, the exact functions/signatures to produce, and a concrete passing assertion — no "TBD"/"handle errors"/"similar to" placeholders remain.

**Type consistency:** `tokenValidator.validate(string) (string, error)`, `newTokenValidator(t, issuer, audience)`, `userStore` getters/seeders, `startMailboxdImpersonation(t, z, extraArgs)`, `z.subjectFor`/`mintUserToken`/`provisionImpersonation`, and the `aud*`/`userA`/`userB` constants are used consistently across all tasks. The shared assertion helpers (`assertSingleMessageSubject`, `assertSingleContact`, `assertSingleEvent`) are defined once (Tasks 3/4/5) and reused.
