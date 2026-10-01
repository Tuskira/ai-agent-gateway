package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

func TestInputSchemaRoundTripPreservesUnknownKeywords(t *testing.T) {
	// The gateway decodes a backend's schema and re-encodes it for its
	// own clients. Anything it drops here is something the model never
	// sees, so the round-trip has to be lossless.
	const in = `{
		"type": "object",
		"title": "Echo input",
		"additionalProperties": false,
		"properties": {"message": {"type": "string"}, "times": {"type": "integer"}},
		"required": ["message"],
		"$defs": {"x": {"type": "string"}}
	}`

	var schema mcp.InputSchema
	if err := json.Unmarshal([]byte(in), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || len(schema.Properties) != 2 || len(schema.Required) != 1 {
		t.Fatalf("modeled fields decoded wrong: %+v", schema)
	}

	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"title", "additionalProperties", "$defs"} {
		if _, ok := out[key]; !ok {
			t.Errorf("re-encoded schema lost %q: %s", key, raw)
		}
	}
}

func TestInputSchemaAlwaysHasAType(t *testing.T) {
	raw, err := json.Marshal(mcp.InputSchema{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"type":"object"}` {
		t.Fatalf("got %s, want {\"type\":\"object\"}", raw)
	}
}

func TestInputSchemaCloneDoesNotAliasTheOriginal(t *testing.T) {
	// Scrubbing overridden arguments mutates a clone; if the clone
	// aliased the original, one connector's scrub would corrupt the
	// cached schema every other caller reads.
	original := mcp.InputSchema{
		Type:       "object",
		Properties: map[string]any{"a": true, "b": true},
		Required:   []string{"a", "b"},
		Extra:      map[string]any{"title": "t"},
	}

	clone := original.Clone()
	delete(clone.Properties, "a")
	clone.Required = clone.Required[:1]
	clone.Extra["title"] = "changed"

	if len(original.Properties) != 2 {
		t.Error("Clone aliased Properties")
	}
	if len(original.Required) != 2 {
		t.Error("Clone aliased Required")
	}
	if original.Extra["title"] != "t" {
		t.Error("Clone aliased Extra")
	}
}
