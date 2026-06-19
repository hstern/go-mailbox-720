# go-mailbox-720

An open-source Go server that implements the **mailbox slice of the Microsoft
Graph API** — so a self-hosted mail/calendar/contacts backend can answer Graph
clients.

> **Status: early / work in progress.** The codegen pipeline, server skeleton,
> and OIDC authentication are in place; the Graph operations themselves are not
> implemented yet (every route currently returns a Graph `notImplemented`
> error). It does not yet talk to a real mail backend.

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
- **OIDC resource server.** Bring your own external IdP (Keycloak, Authentik,
  Dex, Entra, Kanidm); the server validates bearer JWTs and does not issue them.

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

Run the server (auth disabled — all requests allowed; for local experimentation
only):

```sh
go run ./cmd/mailboxd -addr :8080
# every route answers under /v1.0, e.g.:
curl -i http://localhost:8080/v1.0/me/messages   # 501 notImplemented (for now)
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
| `-auth-scope` | Comma-separated required scopes (matched against `scp`/`scope`/`roles`). |
| `-auth-subject-claim` | Token claim mapped to the mailbox identity (default `sub`). |

With one or more issuers configured the server fails closed: it refuses to start
if an issuer cannot be discovered, and rejects any request without a valid token.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/gen-graph-api` | Build-time codegen: fetch spec → subset → `ogen`. |
| `internal/specsubset` | Prunes the full Graph spec to the mailbox slice. |
| `internal/graph` | Hosts the `go generate` directive; `internal/graph/api` is the generated (git-excluded) package. |
| `internal/server` | HTTP server implementing the generated handler. |
| `internal/auth` | OIDC resource-server middleware. |
| `internal/grapherr` | Graph error-object response shape. |
| `cmd/mailboxd` | The server binary. |

## License

[Apache License 2.0](LICENSE).
