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
//     ogen otherwise rejects as a "complex anyOf";
//   - drop the constant "default" on "@odata.type" properties. Each level of an
//     allOf inheritance chain tags itself with its own @odata.type default
//     (attendee -> attendeeBase -> recipient); ogen flattens the allOf and errors
//     with "schemes have different defaults" when merging them. The tag itself is
//     kept as a plain optional string — only the conflicting default is removed;
//   - collapse a scalar "oneOf" (MS Graph's Edm.Double-as-oneOf:[number, string,
//     ReferenceNumeric] idiom, e.g. outlookGeoCoordinates) to its numeric member;
//     ogen cannot infer a discriminator for such a oneOf and fails generation.
//
// Ported from the spike's subset.py; see HANDOFF.md "Pipeline validated".
package specsubset

import (
	"fmt"
	"maps"
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

// odataTypeProp is the OData type-annotation property whose per-subtype constant
// default breaks ogen's allOf merge; see the package doc and stripXMS.
const odataTypeProp = "@odata.type"

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

	// Surface the delta continuation tokens. Microsoft's spec models the delta()
	// function without $deltatoken/$skiptoken — they live in the opaque
	// @odata.deltaLink/@odata.nextLink URLs the client echoes back — so ogen would
	// not give the handler a way to read them. Add them as inline query params.
	injectDeltaTokenParams(kept)

	// Surface the If-Match precondition header on update (PATCH) operations.
	// Microsoft Graph honours If-Match for optimistic concurrency on PATCH, but its
	// OpenAPI metadata only declares it on DELETE, so ogen would not give the
	// handler a way to read it. Add it as an inline header param (matching the
	// shape Graph already uses on DELETE).
	injectIfMatchParams(kept)

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

	// Surface the @odata.etag concurrency annotation on entities. Graph emits it on
	// every read, but the subset (like the full spec) does not declare it as a
	// property — it is an OData control annotation — so ogen would give the handler
	// no field to populate. Add it to the entity base schema (the root of every
	// allOf chain), modelled like the @odata.type tag already declared there.
	injectODataEtag(out["schemas"])

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

	// Microsoft Graph declares `default: false` on the SendResponse property of the
	// meeting-response actions; ogen would apply it on decode, defeating the
	// documented default of true. Strip it so an omitted field stays unset.
	stripSendResponseDefault(subset)

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

// stripSendResponseDefault recursively removes the `default` key from any schema
// property named SendResponse. Microsoft Graph declares `default: false` on the
// meeting-response actions (accept/decline/tentativelyAccept); ogen would emit a
// setDefaults applying it on decode, so an omitted sendResponse would arrive as
// false and the server would skip the reply — the opposite of Graph's documented
// default (true). Stripping the default lets an omitted field stay unset, which
// the handler treats as true.
func stripSendResponseDefault(node any) {
	switch n := node.(type) {
	case map[string]any:
		if props, ok := n["properties"].(map[string]any); ok {
			if sr, ok := props["SendResponse"].(map[string]any); ok {
				delete(sr, "default")
			}
		}
		for _, v := range n {
			stripSendResponseDefault(v)
		}
	case []any:
		for _, v := range n {
			stripSendResponseDefault(v)
		}
	}
}

// stripXMS flattens nullable-anyOf, removes x-ms-* keys, drops navigation
// properties, and removes discriminator blocks, mutating node in place.
// injectDeltaTokenParams adds the $deltatoken and $skiptoken query parameters to
// the GET operation of any kept path whose key ends in "delta()". They are added
// as inline string params (no $ref), so they pull in no extra components and ride
// along in the kept path item. ogen surfaces them on the params struct (as
// Deltatoken/Skiptoken) so the handler can read the opaque continuation token a
// client echoes back from a prior @odata.deltaLink/@odata.nextLink.
func injectDeltaTokenParams(kept map[string]any) {
	for path, pi := range kept {
		if !strings.HasSuffix(path, "delta()") {
			continue
		}
		item, ok := pi.(map[string]any)
		if !ok {
			continue
		}
		op, ok := item["get"].(map[string]any)
		if !ok {
			continue
		}
		params, _ := op["parameters"].([]any)
		for _, name := range []string{"$deltatoken", "$skiptoken"} {
			params = append(params, map[string]any{
				"name":   name,
				"in":     "query",
				"schema": map[string]any{"type": "string"},
			})
		}
		op["parameters"] = params
	}
}

// injectIfMatchParams adds an "If-Match" header parameter to the patch operation
// of every kept path that has one (and does not already declare it). It is added
// as an inline optional string param (no $ref), so it pulls in no extra
// components and rides along in the kept path item. ogen surfaces it on the
// params struct (as IfMatch OptString) so the update handler can read the ETag a
// client supplies for optimistic concurrency — the backing for a conditional
// update (the ConditionalWriter capabilities). It mirrors the shape Graph already
// declares for If-Match on the DELETE operations.
func injectIfMatchParams(kept map[string]any) {
	for _, pi := range kept {
		item, ok := pi.(map[string]any)
		if !ok {
			continue
		}
		op, ok := item["patch"].(map[string]any)
		if !ok {
			continue
		}
		params, _ := op["parameters"].([]any)
		if hasHeaderParam(params, "If-Match") {
			continue
		}
		params = append(params, map[string]any{
			"name":     "If-Match",
			"in":       "header",
			"required": false,
			"schema":   map[string]any{"type": "string"},
		})
		op["parameters"] = params
	}
}

// hasHeaderParam reports whether params already declares a header parameter with
// the given name (case-insensitive, per RFC 9110 header semantics).
func hasHeaderParam(params []any, name string) bool {
	for _, p := range params {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		in, _ := pm["in"].(string)
		pn, _ := pm["name"].(string)
		if in == "header" && strings.EqualFold(pn, name) {
			return true
		}
	}
	return false
}

// injectODataEtag adds an optional "@odata.etag" string property to the entity
// base schema, so every entity DTO ogen generates carries the OData
// optimistic-concurrency annotation Graph emits on reads. The subset drops it
// because it is an OData control annotation rather than a declared property;
// without this the handler would have no field to surface the ETag that backs a
// client's later If-Match. Modelled like the @odata.type tag the spec already
// declares on entity, and left optional (not added to required).
func injectODataEtag(schemas map[string]any) {
	ent, ok := schemas["microsoft.graph.entity"].(map[string]any)
	if !ok {
		return
	}
	props, ok := ent["properties"].(map[string]any)
	if !ok {
		return
	}
	if _, exists := props["@odata.etag"]; !exists {
		props["@odata.etag"] = map[string]any{"type": "string"}
	}
}

func stripXMS(node any) {
	switch n := node.(type) {
	case map[string]any:
		flattenNullableAnyOf(n)
		flattenScalarOneOf(n)
		for k := range n {
			if strings.HasPrefix(k, "x-ms-") {
				delete(n, k)
			}
		}
		// Drop OData navigation properties — the main closure-explosion vector —
		// and strip the conflicting default off the @odata.type tag. Done before
		// recursing, while the markers are still on the child nodes.
		if props, ok := n["properties"].(map[string]any); ok {
			for pname, pval := range props {
				pv, ok := pval.(map[string]any)
				if !ok {
					continue
				}
				if nav, _ := pv["x-ms-navigationProperty"].(bool); nav {
					delete(props, pname)
					continue
				}
				if pname == odataTypeProp {
					delete(pv, "default")
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

// scalarTypes are the OpenAPI primitive types flattenScalarOneOf collapses to.
var scalarTypes = map[string]bool{"number": true, "integer": true, "string": true, "boolean": true}

// flattenScalarOneOf collapses the MS Graph "scalar with alternate encodings"
// oneOf to its numeric member, copying that member's keywords onto node and
// dropping oneOf. Reports whether it rewrote the node.
//
// MS Graph models Edm.Double as oneOf:[{type:number}, {type:string},
// {$ref:ReferenceNumeric}] so IEEE special values ("Infinity"/"NaN") can ride as
// strings (e.g. every outlookGeoCoordinates field). ogen cannot infer a
// discriminator for such a oneOf and fails generation outright. The subset is
// deliberately non-polymorphic, so we keep the primary numeric form and drop the
// string/ReferenceNumeric alternates — lossless for real coordinate data.
//
// Like flattenNullableAnyOf, it is conservative: it fires only when it can
// account for every member as part of the idiom — a primitive scalar, or a bare
// $ref alternate encoding — and only when a numeric (number/integer) arm exists,
// which it selects explicitly so the result does not depend on member order. A
// member with its own object/array shape is a genuine heterogeneous union, not
// this idiom; we refuse to collapse it (return false) so it surfaces as a loud
// ogen error rather than a silently dropped branch.
func flattenScalarOneOf(node map[string]any) bool {
	oneOf, ok := node["oneOf"].([]any)
	if !ok {
		return false
	}
	var numeric map[string]any
	for _, m := range oneOf {
		sm, ok := m.(map[string]any)
		if !ok {
			return false
		}
		switch {
		case isScalarSchema(sm):
			if t, _ := sm["type"].(string); (t == "number" || t == "integer") && numeric == nil {
				numeric = sm
			}
		case isBareRef(sm):
			// Alternate scalar encoding (ReferenceNumeric); intentionally dropped.
		default:
			return false // object/array/complex member: not the scalar idiom.
		}
	}
	if numeric == nil {
		return false
	}
	delete(node, "oneOf")
	maps.Copy(node, numeric)
	return true
}

// isScalarSchema reports whether s describes a primitive scalar (optionally
// nullable, with a format) and carries no object/array structure.
func isScalarSchema(s map[string]any) bool {
	t, ok := s["type"].(string)
	if !ok || !scalarTypes[t] {
		return false
	}
	_, hasProps := s["properties"]
	_, hasItems := s["items"]
	return !hasProps && !hasItems
}

// isBareRef reports whether s is a lone $ref to another component.
func isBareRef(s map[string]any) bool {
	_, ok := s["$ref"]
	return ok
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
