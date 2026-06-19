// Package graph hosts the build-time code generation for the Microsoft Graph
// mailbox API surface.
//
// The generator (cmd/gen-graph-api) fetches the upstream Graph OpenAPI spec,
// prunes it to the mailbox slice (internal/specsubset), and runs ogen. Its
// output is the api subpackage (internal/graph/api), which is deliberately
// git-excluded and regenerated rather than committed — see HANDOFF.md
// "Repo hygiene". This package itself holds only the generate directive and the
// runtime-dependency pin (runtime_deps.go); it imports nothing from api, so the
// tree builds cleanly on a fresh clone before any code has been generated.
//
// Regenerate with:
//
//	go generate ./internal/graph
package graph

//go:generate go run github.com/hstern/go-mailbox-720/cmd/gen-graph-api
