package specsubset

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// sampleSpec exercises every prune in one document:
//   - /me/events is a non-kept path (its schema must not appear);
//   - message.sender uses the nullable-anyOf idiom -> recipient is pulled;
//   - message.attachments is a navigation property -> attachment is NOT pulled;
//   - message carries a discriminator -> eventMessage mapping is NOT followed;
//   - message has an x-ms-* extension that must be stripped.
const sampleSpec = `
openapi: "3.0.4"
info:
  title: sample
  version: v1.0
paths:
  /me/messages:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/microsoft.graph.message'
  /me/events:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/microsoft.graph.event'
components:
  schemas:
    microsoft.graph.message:
      type: object
      x-ms-fake: drop-me
      discriminator:
        propertyName: '@odata.type'
        mapping:
          fake: '#/components/schemas/microsoft.graph.eventMessage'
      properties:
        '@odata.type':
          type: string
          default: '#microsoft.graph.message'
        subject:
          type: string
        sender:
          anyOf:
            - $ref: '#/components/schemas/microsoft.graph.recipient'
            - type: object
              nullable: true
        attachments:
          x-ms-navigationProperty: true
          type: array
          items:
            $ref: '#/components/schemas/microsoft.graph.attachment'
    microsoft.graph.recipient:
      type: object
      properties:
        emailAddress:
          type: string
        latitude:
          description: a ReferenceNumeric-idiom double
          oneOf:
            - format: double
              nullable: true
              type: number
            - nullable: true
              type: string
            - $ref: '#/components/schemas/ReferenceNumeric'
    microsoft.graph.attachment:
      type: object
    microsoft.graph.event:
      type: object
    microsoft.graph.eventMessage:
      type: object
`

// parse re-reads the produced subset so tests can assert on its structure.
func parse(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("subset is not valid YAML: %v", err)
	}
	return m
}

func schemas(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	comps, _ := out["components"].(map[string]any)
	s, _ := comps["schemas"].(map[string]any)
	return s
}

func TestSubsetPrunes(t *testing.T) {
	res, err := Subset([]byte(sampleSpec), Config{KeepPaths: []string{"/me/messages"}})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if res.Paths != 1 {
		t.Errorf("Paths = %d, want 1", res.Paths)
	}

	out := parse(t, res.YAML)
	paths, _ := out["paths"].(map[string]any)
	if _, ok := paths["/me/messages"]; !ok {
		t.Error("kept path /me/messages missing from output")
	}
	if _, ok := paths["/me/events"]; ok {
		t.Error("non-kept path /me/events leaked into output")
	}

	sc := schemas(t, out)
	if _, ok := sc["microsoft.graph.message"]; !ok {
		t.Error("message schema missing")
	}
	if _, ok := sc["microsoft.graph.recipient"]; !ok {
		t.Error("recipient missing: nullable-anyOf flatten should have pulled it via message.sender")
	}
	if _, ok := sc["microsoft.graph.attachment"]; ok {
		t.Error("attachment present: it is reachable only through a navigation property and must be pruned")
	}
	if _, ok := sc["microsoft.graph.eventMessage"]; ok {
		t.Error("eventMessage present: discriminator mapping should not be followed")
	}
	if _, ok := sc["microsoft.graph.event"]; ok {
		t.Error("event present: it belongs to a non-kept path")
	}

	msg, _ := sc["microsoft.graph.message"].(map[string]any)
	if _, ok := msg["x-ms-fake"]; ok {
		t.Error("x-ms-* extension was not stripped from message")
	}
	if _, ok := msg["discriminator"]; ok {
		t.Error("discriminator was not stripped from message")
	}
	props, _ := msg["properties"].(map[string]any)
	if _, ok := props["attachments"]; ok {
		t.Error("navigation property message.attachments was not dropped")
	}
	odata, ok := props["@odata.type"].(map[string]any)
	if !ok {
		t.Error("@odata.type property was removed; only its default should be stripped")
	} else if _, ok := odata["default"]; ok {
		t.Error("@odata.type default was not stripped (ogen allOf-merge conflict)")
	}
	sender, _ := props["sender"].(map[string]any)
	if _, ok := sender["anyOf"]; ok {
		t.Error("message.sender still has anyOf; nullable-anyOf was not flattened")
	}
	if ref, _ := sender["$ref"].(string); ref != "#/components/schemas/microsoft.graph.recipient" {
		t.Errorf("message.sender $ref = %q, want the recipient ref", ref)
	}

	// recipient.latitude uses the scalar-oneOf (ReferenceNumeric) idiom: it must
	// collapse to the numeric member, and the ReferenceNumeric ref must not be
	// followed into the closure.
	rcpt, _ := sc["microsoft.graph.recipient"].(map[string]any)
	rprops, _ := rcpt["properties"].(map[string]any)
	lat, _ := rprops["latitude"].(map[string]any)
	if _, ok := lat["oneOf"]; ok {
		t.Error("recipient.latitude still has oneOf; scalar-oneOf was not collapsed")
	}
	if typ, _ := lat["type"].(string); typ != "number" {
		t.Errorf("recipient.latitude type = %q, want number", typ)
	}
	if _, ok := sc["ReferenceNumeric"]; ok {
		t.Error("ReferenceNumeric present: the collapsed oneOf alternate must not be followed")
	}
}

func TestFlattenScalarOneOf(t *testing.T) {
	num := func() map[string]any { return map[string]any{"type": "number", "format": "double", "nullable": true} }
	str := func() map[string]any { return map[string]any{"type": "string", "nullable": true} }
	ref := func() map[string]any { return map[string]any{"$ref": "#/components/schemas/ReferenceNumeric"} }
	obj := func() map[string]any { return map[string]any{"type": "object", "properties": map[string]any{}} }

	cases := []struct {
		name      string
		oneOf     []any
		collapsed bool // true => oneOf removed and node typed number
	}{
		{"reference-numeric idiom", []any{num(), str(), ref()}, true},
		{"number not first", []any{str(), ref(), num()}, true},
		{"object member refuses collapse", []any{num(), obj()}, false},
		{"no numeric arm refuses collapse", []any{str(), ref()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := map[string]any{"description": "x", "oneOf": tc.oneOf}
			got := flattenScalarOneOf(node)
			if got != tc.collapsed {
				t.Fatalf("flattenScalarOneOf = %v, want %v", got, tc.collapsed)
			}
			_, stillOneOf := node["oneOf"]
			if tc.collapsed {
				if stillOneOf {
					t.Error("oneOf not removed after collapse")
				}
				if node["type"] != "number" {
					t.Errorf("collapsed node type = %v, want number", node["type"])
				}
			} else if !stillOneOf {
				t.Error("oneOf removed despite refusing to collapse")
			}
		})
	}
}

func TestSubsetDropSchemas(t *testing.T) {
	// recipient is normally pulled via message.sender; dropping it must exclude
	// it even though it is referenced.
	res, err := Subset([]byte(sampleSpec), Config{
		KeepPaths:   []string{"/me/messages"},
		DropSchemas: []string{"microsoft.graph.recipient"},
	})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	sc := schemas(t, parse(t, res.YAML))
	if _, ok := sc["microsoft.graph.recipient"]; ok {
		t.Error("recipient present despite being in DropSchemas")
	}
	if _, ok := sc["microsoft.graph.message"]; !ok {
		t.Error("message missing: dropping recipient should not affect message")
	}
}

func TestSubsetMissingPathWarns(t *testing.T) {
	res, err := Subset([]byte(sampleSpec), Config{KeepPaths: []string{"/me/nope"}})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a warning for a missing path")
	}
	if res.Paths != 0 {
		t.Errorf("Paths = %d, want 0", res.Paths)
	}
}

// closureSpec exercises the non-schema component kinds and a multi-hop schema
// chain (m -> deep -> dropme), which the mailbox-slice paths will rely on once
// KeepPaths grows (the {message-id} path references components/parameters, and
// entity schemas reference each other several hops deep).
const closureSpec = `
openapi: "3.0.4"
info:
  title: closure
  version: v1.0
paths:
  /me/messages:
    get:
      parameters:
        - $ref: '#/components/parameters/top'
      responses:
        "200":
          $ref: '#/components/responses/collection'
        "400":
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/m'
components:
  parameters:
    top:
      name: $top
      in: query
      schema:
        type: integer
  responses:
    collection:
      description: ok
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/m'
  schemas:
    m:
      type: object
      properties:
        deep:
          $ref: '#/components/schemas/deep'
    deep:
      type: object
      properties:
        bad:
          $ref: '#/components/schemas/dropme'
    dropme:
      type: object
`

func TestSubsetPullsNonSchemaComponents(t *testing.T) {
	res, err := Subset([]byte(closureSpec), Config{KeepPaths: []string{"/me/messages"}})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if res.Parameters != 1 {
		t.Errorf("Parameters = %d, want 1 (the $top parameter ref must be followed)", res.Parameters)
	}
	if res.Responses != 1 {
		t.Errorf("Responses = %d, want 1 (the response component ref must be followed)", res.Responses)
	}

	out := parse(t, res.YAML)
	comps, _ := out["components"].(map[string]any)
	params, _ := comps["parameters"].(map[string]any)
	if _, ok := params["top"]; !ok {
		t.Error("parameters/top missing from output")
	}
	// The schema referenced only through the response component must still be
	// reached transitively.
	if _, ok := schemas(t, out)["m"]; !ok {
		t.Error("schema m missing: it is reachable via the response component closure")
	}
}

func TestSubsetDropsTargetMidClosure(t *testing.T) {
	// dropme is reached two hops in (m -> deep -> dropme); the frontier guard
	// must skip it while still keeping the schemas above it in the chain.
	res, err := Subset([]byte(closureSpec), Config{
		KeepPaths:   []string{"/me/messages"},
		DropSchemas: []string{"dropme"},
	})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	sc := schemas(t, parse(t, res.YAML))
	if _, ok := sc["dropme"]; ok {
		t.Error("dropme present: a drop target discovered mid-closure must be skipped")
	}
	for _, want := range []string{"m", "deep"} {
		if _, ok := sc[want]; !ok {
			t.Errorf("schema %q missing: dropping a downstream schema must not prune its ancestors", want)
		}
	}
}

// TestInjectDeltaTokenParams checks that the delta continuation-token params are
// added to a delta() operation and that non-delta paths are left untouched.
func TestInjectDeltaTokenParams(t *testing.T) {
	kept := map[string]any{
		"/me/messages/delta()": map[string]any{
			"get": map[string]any{
				"parameters": []any{map[string]any{"name": "$top", "in": "query"}},
			},
		},
		"/me/messages": map[string]any{
			"get": map[string]any{"parameters": []any{}},
		},
	}
	injectDeltaTokenParams(kept)

	got := map[string]bool{}
	for _, p := range kept["/me/messages/delta()"].(map[string]any)["get"].(map[string]any)["parameters"].([]any) {
		got[p.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"$top", "$deltatoken", "$skiptoken"} {
		if !got[want] {
			t.Errorf("delta op missing param %q; got %v", want, got)
		}
	}
	if plain := kept["/me/messages"].(map[string]any)["get"].(map[string]any)["parameters"].([]any); len(plain) != 0 {
		t.Errorf("non-delta path params modified: %v", plain)
	}
}
