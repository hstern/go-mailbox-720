// Command gen-graph-api builds the Microsoft Graph mailbox API client/server
// from the full Graph OpenAPI spec. It runs three stages, wired behind
// `go generate` (see internal/graph/doc.go):
//
//  1. fetch  — download the upstream Graph OpenAPI spec (~30 MB) to a cache file;
//  2. subset — prune it to the mailbox slice (see the specsubset package);
//  3. ogen   — generate the typed models + server stubs from the subset.
//
// No Microsoft IP is committed: the fetched spec, the derived subset, and the
// ogen-generated code are all build-time artifacts excluded by .gitignore (see
// HANDOFF.md "Repo hygiene"). This overrides the usual Go convention of
// committing `go generate` output.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hstern/go-mailbox-720/internal/specsubset"
)

const (
	// defaultSpecURL is the upstream Graph OpenAPI v1.0 document (master branch,
	// refreshed weekly). ~30 MB single file — see the scaling notes in HANDOFF.md.
	defaultSpecURL = "https://raw.githubusercontent.com/microsoftgraph/msgraph-metadata/master/openapi/v1.0/openapi.yaml"

	// ogen tool, pinned. Run via `go run <pkg>@<version>` so the generator never
	// becomes a build dependency of this module — only its runtime packages are
	// (pinned in internal/graph/runtime_deps.go for the generated code).
	ogenPkg     = "github.com/ogen-go/ogen/cmd/ogen"
	ogenVersion = "v1.22.0"

	// fetchTimeout bounds the whole spec download (connect + body).
	fetchTimeout = 5 * time.Minute
)

// mailboxConfig is the Graph mailbox slice we generate: the five Exchange
// entities (messages, mailFolders, events, calendars, contacts), each as its
// collection and by-id item, under both /me and /users/{user-id}. We stop at the
// direct item — nested navigation paths (mailFolders/{id}/messages,
// calendars/{id}/events, messages/{id}/attachments) are deliberately out of
// scope: they add navigation-property recursion and reopen the attachment
// polymorphism question (see HANDOFF.md scaling caveats and DropSchemas below).
//
// Path keys are the parser's unquoted forms; in the source YAML the {…}-bearing
// keys are single-quoted (braces are YAML flow indicators).
var mailboxConfig = specsubset.Config{
	KeepPaths: []string{
		"/me/messages",
		"/me/messages/{message-id}",
		"/me/mailFolders",
		"/me/mailFolders/{mailFolder-id}",
		"/me/events",
		"/me/events/{event-id}",
		"/me/calendars",
		"/me/calendars/{calendar-id}",
		"/me/contacts",
		"/me/contacts/{contact-id}",
		"/users/{user-id}/messages",
		"/users/{user-id}/messages/{message-id}",
		"/users/{user-id}/mailFolders",
		"/users/{user-id}/mailFolders/{mailFolder-id}",
		"/users/{user-id}/events",
		"/users/{user-id}/events/{event-id}",
		"/users/{user-id}/calendars",
		"/users/{user-id}/calendars/{calendar-id}",
		"/users/{user-id}/contacts",
		"/users/{user-id}/contacts/{contact-id}",
	},
	// Polymorphic attachment subtypes: dropped to keep the closure non-recursive
	// even though the attachments nav-property is already pruned (belt and
	// suspenders — itemAttachment.item chains back to message/event/contact).
	DropSchemas: []string{
		"microsoft.graph.itemAttachment",
		"microsoft.graph.referenceAttachment",
		"microsoft.graph.fileAttachment",
	},
}

type config struct {
	url     string
	spec    string
	out     string
	target  string
	pkg     string
	fetch   bool
	runOgen bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.url, "url", defaultSpecURL, "upstream MS Graph OpenAPI spec URL")
	flag.StringVar(&cfg.spec, "spec", "openapi-full.yaml", "path to read/cache the full spec")
	flag.StringVar(&cfg.out, "out", "openapi-subset.yaml", "path to write the pruned subset")
	flag.StringVar(&cfg.target, "target", "api", "directory for the ogen-generated package")
	flag.StringVar(&cfg.pkg, "package", "api", "package name for the ogen-generated code")
	flag.BoolVar(&cfg.fetch, "fetch", true, "download the spec from -url to -spec (false reuses an existing -spec)")
	flag.BoolVar(&cfg.runOgen, "ogen", true, "run ogen on the subset (false stops after writing -out)")
	flag.Parse()

	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "gen-graph-api:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) error {
	if cfg.fetch {
		fmt.Fprintf(os.Stderr, "fetch: %s -> %s\n", cfg.url, cfg.spec)
		if err := fetchSpec(ctx, cfg.url, cfg.spec); err != nil {
			return fmt.Errorf("fetch spec: %w", err)
		}
	}

	full, err := os.ReadFile(cfg.spec)
	if err != nil {
		return fmt.Errorf("read spec: %w", err)
	}
	res, err := specsubset.Subset(full, mailboxConfig)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if err := os.WriteFile(cfg.out, res.YAML, 0o644); err != nil {
		return fmt.Errorf("write subset: %w", err)
	}
	fmt.Fprintf(os.Stderr, "subset: schemas=%d parameters=%d responses=%d requestBodies=%d paths=%d\n",
		res.Schemas, res.Parameters, res.Responses, res.RequestBodies, res.Paths)

	if cfg.runOgen {
		fmt.Fprintf(os.Stderr, "ogen: %s -> %s (package %s)\n", cfg.out, cfg.target, cfg.pkg)
		if err := runOgen(ctx, cfg.target, cfg.pkg, cfg.out); err != nil {
			return fmt.Errorf("run ogen: %w", err)
		}
	}
	return nil
}

// fetchSpec streams the document at url into dest, replacing it only on success.
// The body is copied straight to disk (never fully buffered): the upstream spec
// is ~30 MB and holding it in memory on top of the subsetter's parse is
// wasteful. The download lands in a sibling temp file that is atomically renamed
// into place at the end, so a dropped connection or timeout mid-body cannot
// poison the cache that a later -fetch=false run reuses.
func fetchSpec(ctx context.Context, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename has moved it away

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// runOgen invokes the pinned ogen via `go run`, generating the package at target
// from the subset. ogen creates target if missing; --clean clears stale output.
func runOgen(ctx context.Context, target, pkg, subset string) error {
	target = filepath.Clean(target)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", ogenArgs(target, pkg, subset)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ogenArgs builds the `go run` argument vector for the pinned ogen invocation:
// `go run <ogenPkg>@<ogenVersion> --target <target> --package <pkg> --clean <subset>`.
func ogenArgs(target, pkg, subset string) []string {
	return []string{
		"run", ogenPkg + "@" + ogenVersion,
		"--target", target,
		"--package", pkg,
		"--clean",
		subset,
	}
}
