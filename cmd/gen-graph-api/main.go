// Command gen-graph-api builds the Microsoft Graph mailbox API subset from the
// full Graph OpenAPI spec, ready for ogen.
//
// It reads a full OpenAPI document and writes the pruned subset (see the
// specsubset package for the pruning rules). Fetching the upstream spec and
// invoking ogen are wired as later stages of the same build step.
//
// No Microsoft IP is committed: both the input spec and the emitted subset are
// build-time artifacts, excluded by .gitignore (see HANDOFF.md "Repo hygiene").
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hstern/go-mailbox-720/internal/specsubset"
)

// mailboxConfig is the Graph mailbox slice we generate. Validated end-to-end on
// the messages paths (see HANDOFF.md); grow the slice by extending KeepPaths
// (mailFolders, events, calendars, contacts, and the /users/{id}/... forms).
var mailboxConfig = specsubset.Config{
	KeepPaths: []string{
		"/me/messages",
		"/me/messages/{message-id}",
	},
	DropSchemas: []string{
		"microsoft.graph.itemAttachment",
		"microsoft.graph.referenceAttachment",
		"microsoft.graph.fileAttachment",
	},
}

func main() {
	in := flag.String("in", "openapi-full.yaml", "path to the full MS Graph OpenAPI spec")
	out := flag.String("out", "openapi-subset.yaml", "path to write the pruned subset")
	flag.Parse()

	if err := run(*in, *out); err != nil {
		fmt.Fprintln(os.Stderr, "gen-graph-api:", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	full, err := os.ReadFile(in)
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
	if err := os.WriteFile(out, res.YAML, 0o644); err != nil {
		return fmt.Errorf("write subset: %w", err)
	}
	fmt.Fprintf(os.Stderr, "subset: schemas=%d parameters=%d responses=%d requestBodies=%d paths=%d\n",
		res.Schemas, res.Parameters, res.Responses, res.RequestBodies, res.Paths)
	return nil
}
