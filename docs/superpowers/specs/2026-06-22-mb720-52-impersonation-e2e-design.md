# MB720-52 — E2E test of the per-identity backend path (RFC 8693 impersonation → backend)

Status: design (approved pending user review)
Date: 2026-06-22
Ticket: MB720-52 (parent: MB720-41 per-identity backend cluster; siblings MB720-42/43/44)

## Problem

The per-identity backend cluster (MB720-41 foundation; MB720-42 JMAP, MB720-43
IMAP/SMTP, MB720-44 CalDAV/CardDAV) is **unit-tested only**. The RFC 8693 token
exchange, the per-identity providers, and each protocol's auth path are covered
with fakes, but nothing exercises the whole chain end-to-end against a real IdP
and a backend that actually validates the exchanged token.

We want an e2e that drives the full path:

1. A real IdP (Zitadel) issues a user access token (`aud` = mailboxd).
2. mailboxd validates it (existing middleware).
3. mailboxd exchanges it (RFC 8693) for a backend-audience token **preserving the
   user's `sub`**, authenticating as the `mailbox` OAuth client.
4. The backend **validates that exchanged token** (issuer + audience + signature)
   and serves **that user's** data.

The defining assertion: two different users each get **their own** mailbox /
calendar / contacts, with **no cross-tenant bleed**.

## Scope

All six per-identity paths exposed by mailboxd's flags:

| Path          | Wire protocol | mailboxd flag           | Backend auth        |
|---------------|---------------|-------------------------|---------------------|
| mail (JMAP)   | JMAP / HTTP   | `-mail-jmap-audience`     | `Authorization: Bearer` |
| mail (IMAP)   | IMAP          | `-mail-imap-audience`     | SASL OAUTHBEARER (authzid = `sub`) |
| calendar      | CalDAV / HTTP | `-cal-caldav-audience`    | `Authorization: Bearer` |
| contacts (CardDAV) | CardDAV / HTTP | `-contacts-carddav-audience` | `Authorization: Bearer` |
| contacts (JMAP) | JMAP / HTTP | `-contacts-jmap-audience` | `Authorization: Bearer` |
| SMTP          | SMTP          | `-smtp-audience`          | SASL OAUTHBEARER (authzid = `sub`) |

Calendar per-identity is **CalDAV only** — there is no `-cal-jmap-audience` flag.

Out of scope: real Stalwart/Dovecot/Radicale OIDC configuration (those backends
authenticate with static credentials in the current harness; standing up their
OIDC-bearer trust is a separate effort). Kanidm (service-account-only token
exchange, cannot impersonate — `idpCaps.impersonation == false`).

## Approach: lightweight token-validating backends, in-process

Rather than configure a real server's OIDC trust, each protocol gets a small
**in-process** fake backend that genuinely validates the exchanged JWT and serves
per-subject data. This proves the exchange→backend contract and the isolation
property without fighting a real server's OIDC config, and needs no new container
images. mailboxd runs as a subprocess (as today) and dials these fakes on
localhost; the fakes reach Zitadel's JWKS on `localhost:8085`.

Reuse: the repo already has httptest-based fake JMAP servers
(`internal/{mail,contacts,calendar}/jmap/jmap_test.go`), fake CalDAV/CardDAV
servers (`internal/calendar/caldav`, `internal/contacts/carddav` tests), a Bearer
DAV helper (`internal/davauth/bearer.go`), and the go-imap/v2 `imapserver`,
go-smtp server, and go-webdav server libraries are already vendored. The fakes
adapt these patterns; they are not written from scratch.

### Components (all in `test/e2e-oidc/`)

1. **`tokenValidator`** (shared). Given a JWT and an expected backend audience:
   fetch Zitadel discovery → `jwks_uri` → keys; verify RS256 signature, `iss`,
   `exp`; assert `aud` contains the expected backend audience; return `sub`.
   This is what enforces "the backend trusts the exchanged token" — a token with
   the wrong audience (e.g. the mailboxd-audience user token presented directly)
   is rejected. Built on the same JWT/JWKS library the `internal/auth` package
   uses, to avoid a second JWT stack.

2. **`userStore`** (shared). Per-`sub` seeded fixtures (messages, calendar
   events, contacts) and recorders (sent SMTP messages). Every fake serves and
   records strictly by the validated `sub`. Two subjects are seeded with
   distinct, recognizable data.

3. **Fake backends** (each = `tokenValidator` + `userStore`):
   - **JMAP HTTP fake** — serves a JMAP session document + the API endpoint,
     handling the method calls the `mailjmap` and `jmapcontacts` clients make
     (mail: `Mailbox/get`, `Email/query`, `Email/get`; contacts: the JSContact
     methods the client uses). Bearer auth. Serves mail-jmap and contacts-jmap.
   - **WebDAV fake** — caldav + carddav, Bearer auth via the `davauth` pattern,
     serving the REPORT/PROPFIND surface the `caldav`/`carddav` clients use.
   - **IMAP fake** — go-imap/v2 `imapserver` with a SASL OAUTHBEARER mechanism;
     minimal SELECT INBOX / FETCH from the store.
   - **SMTP fake** — go-smtp server with SASL OAUTHBEARER; records MAIL FROM /
     RCPT / DATA per validated subject (extends the existing `smtpsink`).

### Zitadel provisioning additions (`zitadel_test.go`)

- Grant the `mailbox` machine user the `IAM_END_USER_IMPERSONATOR` instance role
  and the token-exchange grant. (`enableImpersonation` is already set in
  `provision()`.)
- Register the per-protocol backend audiences so exchanged tokens carry the
  expected `aud`. Decision: **distinct audience per protocol** (exercises the
  real config and proves each provider requests its own audience); fall back to a
  single shared backend audience if per-audience registration proves costly in
  the Zitadel admin API.
- Create **two** subject users (`userA`, `userB`) as machine users — mirroring
  the existing `client_credentials` pattern, so their tokens carry a stable `sub`
  and `aud` = the mailbox audience that mailboxd validates. Add
  `mintUserToken(user)`.

### Test structure: one top-level test per protocol

Each protocol is its own top-level `Test...`, so each is an independent CI job
that can fail or skip on its own (mirroring the existing per-`-run` CI targets
and the ticket's per-protocol granularity):

- `TestImpersonationJMAPMail`
- `TestImpersonationContactsJMAP`
- `TestImpersonationCalDAV`
- `TestImpersonationCardDAV`
- `TestImpersonationIMAP`
- `TestImpersonationSMTP`

They share one harness helper that: starts Zitadel (once-per-test; reuse the
existing start/provision), starts the relevant fake backend, starts mailboxd with
`-tokenexchange-endpoint` + the protocol's `-*-audience` flag (plus
`MAILBOXD_TOKENEXCHANGE_CLIENT_SECRET`), and returns the mailboxd base URL +
mint/seed handles. All are gated on `idpCaps.impersonation` (Zitadel-only).

### Per-test assertions

For each protocol, with seeded users A and B:

1. Mint userA's token → call the client-facing route → assert the response is
   **A's** seeded data.
2. Mint userB's token → same route → assert **B's** data.
3. Assert A's response never contains B's data (no cross-tenant bleed).
4. Negative: present a wrong-audience token (e.g. the mailboxd-audience token
   straight to the backend) → backend rejects → mailboxd surfaces an error.

Client-facing routes (mailboxd's Graph facade, as in the existing e2e):
`GET /me/messages` (mail JMAP & IMAP), `GET /me/events` (CalDAV),
`GET /me/contacts` (CardDAV & contacts-JMAP). SMTP is driven via the
accept/decline-meeting path that emits a reply email; assert the fake SMTP
recorded a message whose sender is A's subject.

## Staging (each increment independently verifiable; JMAP first per the ticket)

0. **Exchange spike** — prove in-harness that userA's token exchanges to an
   `aud`=backend, `sub`-preserved token. Isolates the top risk (Zitadel
   impersonation provisioning) before any fake is built.
1. Shared `tokenValidator` + `userStore`.
2. **JMAP mail** (`TestImpersonationJMAPMail`) — the ticket's MVP; proves the
   whole chain end-to-end.
3. `TestImpersonationContactsJMAP` (reuses the JMAP fake).
4. `TestImpersonationCalDAV`.
5. `TestImpersonationCardDAV`.
6. `TestImpersonationIMAP`.
7. `TestImpersonationSMTP`.

## CI

Add one CI job per protocol test (alongside the existing `TestOIDCEndToEnd` /
`TestDumbBackendEndToEnd` / `TestStalwart` jobs), each
`cd test/e2e-oidc && go test -timeout <n>s -run TestImpersonation<X> ./...`.
Tests self-skip when docker is unavailable (existing pattern).

## Risks

- **Zitadel impersonation provisioning** is the dominant unknown: whether a
  machine-user subject token can be exchanged with the impersonator role,
  audience shaping for the exchanged token, and `sub` preservation. The exact
  admin-API calls will be iterated against the live container; increment 0
  isolates this before downstream work.
- **JMAP fake surface** — the precise method set the clients call. Mitigated by
  reusing the in-repo httptest fakes as the starting point.
- **go-imap/v2 OAUTHBEARER server support** — confirm the `imapserver` SASL hook
  during increment 6; the SMTP/go-smtp OAUTHBEARER path is already exercised by
  the existing sink.

## Non-goals / YAGNI

- No connection pooling for the fakes; dial-per-request is fine for a test.
- No real backend OIDC config (Stalwart/Dovecot/Radicale).
- No new mailboxd production code — this is test-only; the per-identity providers
  already exist and are wired.
