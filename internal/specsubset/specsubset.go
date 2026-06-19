// Package specsubset reduces the Microsoft Graph OpenAPI spec to a small,
// self-contained subset that ogen can consume.
//
// The full Graph spec is ~30 MB / thousands of schemas. Keeping a handful of
// paths and naively resolving their transitive $ref closure re-explodes back to
// nearly the whole spec, because OData navigation properties and polymorphic
// discriminators chain every entity to every other (e.g. itemAttachment.item ->
// outlookItem -> message/event/contact). Subset prunes those re-exploding edges
// while it walks the closure:
//
//   - drop object properties marked "x-ms-navigationProperty: true" (the main
//     re-explosion vector);
//   - drop every discriminator block (the subset is deliberately
//     non-polymorphic; any subtype a mapping points at has been pruned);
//   - strip all "x-ms-*" vendor extensions;
//   - flatten the OData nullable-$ref idiom
//     (anyOf: [{$ref: X}, {type: object, nullable: true}]) to a bare $ref, which
//     ogen otherwise rejects as a "complex anyOf".
//
// Ported from the spike's subset.py; see HANDOFF.md "Pipeline validated".
package specsubset

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config selects what to keep and what to drop outright.
type Config struct {
	// KeepPaths lists the OpenAPI path templates to retain verbatim; every other
	// entry under "paths" is discarded.
	KeepPaths []string
	// DropSchemas names components/schemas to remove even when referenced — used
	// to cut polymorphic subtypes that are the source of closure recursion.
	DropSchemas []string
}

// Result is the subset plus a summary for logging and verification.
type Result struct {
	YAML          []byte
	Schemas       int
	Parameters    int
	Responses     int
	RequestBodies int
	Paths         int
	Warnings      []string
}

const refPrefix = "#/components/"

var componentKinds = []string{"schemas", "parameters", "responses", "requestBodies"}

// Subset reads a full OpenAPI 3.0 document (YAML) and returns a minimal spec
// containing only cfg.KeepPaths and the pruned transitive $ref closure reachable
// from them.
func Subset(full []byte, cfg Config) (*Result, error) {
	var spec map[string]any
	if err := yaml.Unmarshal(full, &spec); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}

	drop := make(map[string]bool, len(cfg.DropSchemas))
	for _, s := range cfg.DropSchemas {
		drop[s] = true
	}

	src := map[string]map[string]any{}
	comps, _ := spec["components"].(map[string]any)
	for _, kind := range componentKinds {
		if m, ok := comps[kind].(map[string]any); ok {
			src[kind] = m
		} else {
			src[kind] = map[string]any{}
		}
	}

	res := &Result{}

	// 1. Keep only the target paths.
	allPaths, _ := spec["paths"].(map[string]any)
	kept := map[string]any{}
	for _, p := range cfg.KeepPaths {
		if pi, ok := allPaths[p]; ok {
			kept[p] = pi
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf("path %s not found in spec", p))
		}
	}

	// Prune the kept path items before computing the closure.
	for _, pi := range kept {
		stripXMS(pi)
	}

	// 2+3. Breadth-first closure, pruning each component as it is pulled in.
	out := map[string]map[string]any{}
	for _, kind := range componentKinds {
		out[kind] = map[string]any{}
	}

	frontier := map[string]bool{}
	for _, pi := range kept {
		collectRefs(pi, frontier)
	}
	visited := map[string]bool{}

	for len(frontier) > 0 {
		ref := popOne(frontier)
		if visited[ref] {
			continue
		}
		visited[ref] = true

		kind, name, ok := refToLoc(ref)
		if !ok {
			res.Warnings = append(res.Warnings, "non-component ref skipped: "+ref)
			continue
		}
		if kind == "schemas" && drop[name] {
			continue
		}
		node, ok := src[kind][name]
		if !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("missing component %s/%s", kind, name))
			continue
		}

		stripXMS(node)
		out[kind][name] = node

		refs := map[string]bool{}
		collectRefs(node, refs)
		for r := range refs {
			if k, n, ok := refToLoc(r); ok && k == "schemas" && drop[n] {
				continue
			}
			if !visited[r] {
				frontier[r] = true
			}
		}
	}

	// 4. Assemble the minimal spec.
	components := map[string]any{}
	for _, kind := range componentKinds {
		if len(out[kind]) > 0 {
			components[kind] = out[kind]
		}
	}
	openapi := spec["openapi"]
	if openapi == nil {
		openapi = "3.0.4"
	}
	servers := spec["servers"]
	if servers == nil {
		servers = []any{map[string]any{"url": "https://graph.microsoft.com/v1.0"}}
	}
	subset := map[string]any{
		"openapi": openapi,
		"info": map[string]any{
			"title":   "MS Graph mailbox subset",
			"version": "v1.0",
		},
		"servers":    servers,
		"paths":      kept,
		"components": components,
	}

	data, err := yaml.Marshal(subset)
	if err != nil {
		return nil, fmt.Errorf("marshal subset: %w", err)
	}

	res.YAML = data
	res.Schemas = len(out["schemas"])
	res.Parameters = len(out["parameters"])
	res.Responses = len(out["responses"])
	res.RequestBodies = len(out["requestBodies"])
	res.Paths = len(kept)
	return res, nil
}

// stripXMS flattens nullable-anyOf, removes x-ms-* keys, drops navigation
// properties, and removes discriminator blocks, mutating node in place.
func stripXMS(node any) {
	switch n := node.(type) {
	case map[string]any:
		flattenNullableAnyOf(n)
		for k := range n {
			if strings.HasPrefix(k, "x-ms-") {
				delete(n, k)
			}
		}
		// Drop OData navigation properties — the main closure-explosion vector.
		// Done before recursing, while the marker is still on the child node.
		if props, ok := n["properties"].(map[string]any); ok {
			for pname, pval := range props {
				if pv, ok := pval.(map[string]any); ok {
					if nav, _ := pv["x-ms-navigationProperty"].(bool); nav {
						delete(props, pname)
					}
				}
			}
		}
		delete(n, "discriminator")
		for _, v := range n {
			stripXMS(v)
		}
	case []any:
		for _, v := range n {
			stripXMS(v)
		}
	}
}

// flattenNullableAnyOf collapses the OData nullable-$ref idiom
// (anyOf: [{$ref: X}, {type: object, nullable: true}]) to a bare $ref on node,
// dropping the explicit nullability. Reports whether it rewrote the node.
func flattenNullableAnyOf(node map[string]any) bool {
	anyOf, ok := node["anyOf"].([]any)
	if !ok {
		return false
	}
	var refVal any
	refs, nullers := 0, 0
	for _, s := range anyOf {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if r, has := sm["$ref"]; has {
			refs++
			refVal = r
		} else if isNullObject(sm) {
			nullers++
		}
	}
	if refs == 1 && refs+nullers == len(anyOf) {
		delete(node, "anyOf")
		node["$ref"] = refVal
		return true
	}
	return false
}

func isNullObject(m map[string]any) bool {
	nullable, _ := m["nullable"].(bool)
	typ, _ := m["type"].(string)
	return nullable && typ == "object"
}

// collectRefs gathers every $ref string reachable from node.
func collectRefs(node any, acc map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			acc[ref] = true
		}
		for _, v := range n {
			collectRefs(v, acc)
		}
	case []any:
		for _, v := range n {
			collectRefs(v, acc)
		}
	}
}

// refToLoc splits "#/components/<kind>/<name>" into (kind, name). Component
// names contain no slashes, so a single split on the kind boundary suffices.
func refToLoc(ref string) (kind, name string, ok bool) {
	rest, found := strings.CutPrefix(ref, refPrefix)
	if !found {
		return "", "", false
	}
	k, n, found := strings.Cut(rest, "/")
	if !found {
		return "", "", false
	}
	return k, n, true
}

func popOne(set map[string]bool) string {
	for k := range set {
		delete(set, k)
		return k
	}
	return ""
}
