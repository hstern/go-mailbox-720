# go-mailbox-720

An open-source Go server that implements the **mailbox slice of the Microsoft
Graph API** — so a self-hosted mail/calendar/contacts backend can answer Graph
clients.

> **Status: working, still maturing.** Reads and writes for mail, calendar, and
> contacts are implemented over real IMAP / CalDAV / CardDAV backends, behind
> OIDC, with `$filter`, `$batch`, delta sync, and change-notification
> subscriptions. The iTIP/iMIP scheduling engine and several protocol corners are
> built but not yet fully wired (see [Status detail](#status-detail)). Without a
> backend configured, an operation returns a Graph `notImplemented` (501).

"Microsoft Graph" and "Microsoft" are trademarks of Microsoft Corporation, used
here descriptively (nominative fair use) to say what this software is compatible
with. This project is not affiliated with or endorsed by Microsoft.

## What it is

- **Server only, no client.** The client side is already solved by Microsoft's
  official, Kiota-generated [`msgraph-sdk-go`](https://github.com/microsoftgraph/msgraph-sdk-go),
  which also doubles as the conformance harness.
- **Scope: the Exchange/mailbox slice** — `messages`, `mailFolders`, `events`,
  `calendars`, `contacts`, under `/me` and `/users/{id}`. Everything else in
  Graph (Teams, SharePoint, OneDrive, directory, …) is out of scope.
- **Bring your own backends.** Mail maps onto **IMAP**, calendar onto **CalDAV**,
  contacts onto **CardDAV** — your existing self-hosted servers (e.g. Dovecot +
  Radicale). Backend-neutral ports mean a JMAP adapter can drop in later.
- **OIDC resource server.** Bring your own external IdP (Keycloak, Authentik,
  Dex, Entra, Kanidm); the server validates bearer tokens and does not issue them.

## Implemented

| Area | Operations |
| --- | --- |
| Mail (IMAP) | `GET /me/messages` (with `$filter` → IMAP SEARCH), `GET /me/mailFolders`, `GET /me/messages/{id}`, `PATCH` (isRead) + `DELETE`, `GET /me/messages/delta` (incremental sync) |
| Calendar (CalDAV) | `GET /me/events`, `GET /me/calendars`, `POST` / `PATCH` / `DELETE /me/events`, `GET /me/events/delta`, `POST /me/events/{id}/accept` / `decline` / `tentativelyAccept` (iMIP reply) |
| Contacts (CardDAV) | `GET /me/contacts`, `POST` / `PATCH` / `DELETE /me/contacts`, `GET /me/contacts/delta` |
| Protocol | `POST /$batch` (JSON batching), `POST` / `GET` / `DELETE /v1.0/subscriptions` (change notifications, with IMAP-IDLE-driven delivery) |
| Auth | JWT (JWKS) + opaque-token (RFC 7662 introspection) validation, RFC 9493 subject identity |

Event times accept UTC, RFC3339 offsets, and Windows zone names (e.g. `Pacific
Standard Time`), resolved to the correct instant. Secrets (IMAP/CalDAV/CardDAV
passwords, the introspection client secret) are read from the environment, never
flags, so they stay out of the process table.

## Status detail

Built but not yet fully wired, or deliberately first-cut:

- **iTIP/iMIP scheduling** (`internal/scheduling`, `internal/itip`, `internal/smtp`,
  `internal/schedrun`): the full engine (parse, reply, request/cancel, iMIP email
  composition) plus both directions wired — `POST /me/events/{id}/accept|decline|
  tentativelyAccept` email an iMIP reply to the organizer (needs `-smtp-addr` +
  `-mailbox-email`), and an opt-in inbound trigger loop (`-enable-scheduling`) turns
  REQUEST mail into tentative calendar events. Both honor the RFC 6638
  `calendar-auto-schedule` capability switch: when the CalDAV server schedules
  natively the trigger stands down, and accept/decline record the responder's
  PARTSTAT via CalDAV (the server sends the reply) instead of emailing. Still open:
  recording `responseStatus` locally on the storage-only (email) path, and an
  end-to-end test against a real RFC 6638 server (the Docker matrix has none yet).
- **Delta**: all three delta endpoints (`/me/messages`, `/me/events`,
  `/me/contacts`) report created/updated items **and** `@removed` tombstones for
  deletions. Mail uses IMAP **CONDSTORE/QRESYNC** (RFC 7162) — `CHANGEDSINCE` for
  new + flag/read-state changes, `VANISHED` for expungements — and **requires a
  QRESYNC-capable server** (it returns 501 otherwise, rather than silently
  degrading). Calendar/contacts use CalDAV/CardDAV **sync-collection** (RFC 6578).
  Mail delta consumes QRESYNC client support from a go-imap fork via a `go.mod`
  replace, pending upstream [emersion/go-imap#757](https://github.com/emersion/go-imap/pull/757).
- **Subscriptions**: single-tenant in-memory store (per-identity keying is a
  prerequisite before multi-mailbox use); notification delivery is created-only.
- **Update PATCH** of events/contacts merges provided fields; partial collection
  semantics are best-effort.

## Repository hygiene: no Microsoft IP in git

The Microsoft Graph OpenAPI spec, the pruned subset derived from it, and the
[`ogen`](https://github.com/ogen-go/ogen)-generated code (a derivative of the
spec) are **fetched and generated at build time, never committed.** This is why
you must run `go generate` before building (below), and it deliberately
overrides the usual Go convention of committing `go generate` output.

## Build & run

The generated API package is produced from the upstream spec at build time, so
generate it first (this fetches the ~30 MB Graph OpenAPI document and runs
`ogen` — it needs network access):

```sh
go generate ./internal/graph
go build ./...
go test ./...
```

Some integration tests need Docker and are behind a `dockertest` build tag so the
default `go test ./...` stays fast. They run the adapters against real servers:

```sh
go test -tags dockertest ./internal/mail/imap/        # IMAP + delta + IDLE vs Dovecot
go test -tags dockertest ./internal/calendar/caldav/  # CalDAV read/write vs Radicale
```

Run the server against your backends (auth disabled here — local experimentation
only). With a backend configured the routes return real data:

```sh
MAILBOXD_IMAP_PASSWORD=… MAILBOXD_CALDAV_PASSWORD=… MAILBOXD_CARDDAV_PASSWORD=… \
go run ./cmd/mailboxd -addr :8080 \
  -mail-imap-addr imap.example.com:993 -mail-imap-username alice \
  -cal-caldav-url https://dav.example.com/ -cal-caldav-username alice \
  -contacts-carddav-url https://dav.example.com/ -contacts-carddav-username alice
curl -i http://localhost:8080/v1.0/me/messages   # 200 with the inbox
```

Run with OIDC enforced (the production posture):

```sh
go run ./cmd/mailboxd \
  -addr :8080 \
  -auth-issuer https://idp.example.com/realms/main \
  -auth-audience mailbox-api \
  -auth-scope Mail.Read \
  -auth-subject-claim sub
```

| Flag | Meaning |
| --- | --- |
| `-auth-issuer` | Comma-separated trusted OIDC issuer URL(s). Empty disables auth. |
| `-auth-audience` | Expected token audience (`aud`). |
| `-auth-scope` | Comma-separated required scopes. |
| `-auth-scope-claims` | Comma-separated claims that carry granted scopes (default `scope,roles`; Microsoft Entra: `scope,scp,roles`). |
| `-auth-subject-claim` | Token claim mapped to the mailbox identity (default `sub`). |
| `-auth-introspect-client-id` | OAuth2 client id enabling RFC 7662 introspection of **opaque** tokens (secret via `MAILBOXD_INTROSPECT_CLIENT_SECRET`). |
| `-auth-resource` | This resource's identifier URL (RFC 8707); when set, publishes RFC 9728 protected-resource metadata at `/.well-known/oauth-protected-resource`. |
| `-mail-imap-addr` / `-mail-imap-username` / `-mail-imap-tls` | IMAP mail backend (password via `MAILBOXD_IMAP_PASSWORD`). |
| `-cal-caldav-url` / `-cal-caldav-username` | CalDAV calendar backend (password via `MAILBOXD_CALDAV_PASSWORD`). |
| `-contacts-carddav-url` / `-contacts-carddav-username` | CardDAV contacts backend (password via `MAILBOXD_CARDDAV_PASSWORD`). |

JWT bearer tokens are validated locally: the JWS is verified against the issuer's
JWKS (signature, the issuer's discovery-advertised algorithms, audience, expiry) —
the access-control gate. The token's `typ` then selects how strictly it is decoded:
a token that declares itself an **RFC 9068** access token (`typ=at+jwt`) is held to
that profile (the §2.2 required claims, incl. `jti`/`client_id`), while a plain
`typ=JWT` token — as Microsoft Entra and many IdPs issue — is accepted on signature
+ audience like any bearer JWT. Granted scopes are read from the claims named by
`-auth-scope-claims` (default `scope` — the RFC 8693 §4.2 / RFC 9068 §2.2.3 claim —
plus app `roles`). **Microsoft Entra / Azure AD** does not use the standard claim:
it carries delegated permissions in a non-standard `scp` claim, so an Entra-fronted
deployment should set `-auth-scope-claims scope,scp,roles` (`scp` is opt-in because
it is not an RFC- or IANA-registered claim). Opaque access tokens (e.g. Kanidm's
default) are validated via **RFC 7662**
introspection when `-auth-introspect-client-id` is set; for introspected tokens the
audience is enforced when the introspection response carries an `aud` (otherwise the
resource server's own introspection credentials are the binding). The mailbox
identity is an RFC 9493 `iss_sub` Subject Identifier (issuer + subject), so
mailboxes stay distinct across multiple issuers. (JWT decode/validation uses
`hstern/go-access-tokens`; introspection uses `hstern/go-token-introspection`.)

With one or more issuers configured the server fails closed: it refuses to start
if an issuer cannot be discovered, and rejects any request without a valid token.
A rejection carries an **RFC 6750** `WWW-Authenticate: Bearer` challenge
(`invalid_token` / `insufficient_scope`), and — with `-auth-resource` set — the
server publishes **RFC 9728** protected-resource metadata at
`/.well-known/oauth-protected-resource` so a client can discover the trusted issuers
and scopes; the challenge then points at it via the §5.1 `resource_metadata`
parameter. (The challenge is built with `hstern/go-bearer-token`, the metadata served
and linked by `hstern/go-protected-resource-metadata`.)

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/gen-graph-api`, `internal/specsubset` | Build-time codegen: fetch spec → subset → `ogen`. |
| `internal/graph` | Hosts the `go generate` directive; `internal/graph/api` is the generated (git-excluded) package. |
| `internal/server` | HTTP server implementing the generated handler (mail/calendar/contacts/delta/batch handlers + Graph↔neutral mapping). |
| `internal/auth`, `internal/grapherr` | OIDC resource-server middleware; Graph error-object shape. |
| `internal/odata`, `internal/batch` | `$filter` parsing/translation; `$batch` JSON batching. |
| `internal/mail` + `internal/mail/imap` | Mail port + IMAP adapter (read/write/delta/IDLE). |
| `internal/calendar` + `…/caldav` | Calendar port + CalDAV adapter (read/write). |
| `internal/contacts` + `…/carddav` | Contacts port + CardDAV adapter (read/write). |
| `internal/subscriptions`, `internal/notify` | Change-notification model/store/validation/delivery; IDLE→delta→deliver loop. |
| `internal/scheduling`, `internal/itip`, `internal/smtp` | iTIP/iMIP engine, scheduling orchestration, SMTP send port. |
| `internal/tz` | Graph (Windows/IANA) time-zone resolution. |
| `cmd/mailboxd` | The server binary. |
| `test/conformance` | Black-box conformance test driving mailboxd with the official `msgraph-sdk-go` client (separate module). |
| `test/e2e-oidc` | End-to-end: real Kanidm + Dovecot + Radicale (Docker) — an authenticated client reads mail and calendar through mailboxd (separate module). |

## License

[Apache License 2.0](LICENSE).
