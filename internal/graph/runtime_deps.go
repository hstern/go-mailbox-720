//go:build graphapideps

// This file is never compiled into the module — its build tag (graphapideps) is
// never set. It exists solely so `go mod tidy` keeps the ogen runtime modules
// that the generated, git-excluded internal/graph/api package imports. Without
// it, a fresh clone (which has no generated code) would have `go mod tidy` strip
// those requirements from go.mod, and the next `go generate` would produce code
// that does not build. This is the standard tools.go pattern applied to runtime
// dependencies of generated-but-unchecked-in code.
//
// This must mirror the *full* import set of the ogen output, not just one
// package per module: `go mod tidy` records go.sum zip hashes only for packages
// in its build closure, so a narrower list here (e.g. only ogen/middleware)
// would let tidy prune the hashes that ogen/http transitively needs
// (golang.org/x/sync/errgroup, golang.org/x/net/http/httpguts), breaking the
// post-generate build. Keep in sync with the imports of internal/graph/api.
package graph

import (
	_ "github.com/go-faster/errors"
	_ "github.com/go-faster/jx"
	_ "github.com/ogen-go/ogen/conv"
	_ "github.com/ogen-go/ogen/http"
	_ "github.com/ogen-go/ogen/json"
	_ "github.com/ogen-go/ogen/middleware"
	_ "github.com/ogen-go/ogen/ogenerrors"
	_ "github.com/ogen-go/ogen/ogenregex"
	_ "github.com/ogen-go/ogen/otelogen"
	_ "github.com/ogen-go/ogen/uri"
	_ "github.com/ogen-go/ogen/validate"
	_ "go.opentelemetry.io/otel"
	_ "go.opentelemetry.io/otel/attribute"
	_ "go.opentelemetry.io/otel/codes"
	_ "go.opentelemetry.io/otel/metric"
	_ "go.opentelemetry.io/otel/semconv/v1.39.0"
	_ "go.opentelemetry.io/otel/trace"
)
