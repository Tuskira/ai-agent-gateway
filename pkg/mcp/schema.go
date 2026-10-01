package mcp

import "encoding/json"

// schemaKnown are the InputSchema keywords that have their own struct
// field. Everything else a backend sends is kept in Extra.
var schemaKnown = map[string]struct{}{
	"type":       {},
	"properties": {},
	"required":   {},
}

// UnmarshalJSON decodes a tool input schema, keeping every keyword the
// struct does not model explicitly in Extra.
//
// The gateway decodes a backend's tool list and re-encodes it for its own
// clients, so any keyword it drops on the floor is a keyword the client
// never sees. A plain struct decode silently loses additionalProperties,
// $defs, enum-bearing sub-schemas at the top level, titles and
// descriptions -- all of which change how a model calls the tool. Keeping
// the remainder verbatim makes the round-trip lossless.
func (s *InputSchema) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*s = InputSchema{}

	if v, ok := raw["type"]; ok {
		if err := json.Unmarshal(v, &s.Type); err != nil {
			return err
		}
	}
	if v, ok := raw["properties"]; ok {
		if err := json.Unmarshal(v, &s.Properties); err != nil {
			return err
		}
	}
	if v, ok := raw["required"]; ok {
		if err := json.Unmarshal(v, &s.Required); err != nil {
			return err
		}
	}

	for key, v := range raw {
		if _, known := schemaKnown[key]; known {
			continue
		}
		var any0 any
		if err := json.Unmarshal(v, &any0); err != nil {
			return err
		}
		if s.Extra == nil {
			s.Extra = make(map[string]any, len(raw))
		}
		s.Extra[key] = any0
	}

	return nil
}

// MarshalJSON re-encodes the schema, merging Extra back in. A key in
// Extra can never shadow a modeled keyword: the three known keys are
// written last.
func (s InputSchema) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(s.Extra)+3)
	for k, v := range s.Extra {
		if _, known := schemaKnown[k]; known {
			continue
		}
		out[k] = v
	}

	// An MCP tool schema is always an object; default rather than emit a
	// schema with no "type", which some clients reject outright.
	typ := s.Type
	if typ == "" {
		typ = "object"
	}
	out["type"] = typ

	if s.Properties != nil {
		out["properties"] = s.Properties
	}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	}

	return json.Marshal(out)
}

// Clone returns a deep-enough copy of the schema for the gateway's one
// mutation: scrubbing gateway-stamped argument names out of Properties
// and Required before a tool is advertised. The maps and the slice are
// copied so a scrub can never mutate a cached or shared schema; the
// property values themselves are shared, as nothing ever writes into
// them.
func (s InputSchema) Clone() InputSchema {
	out := InputSchema{Type: s.Type}

	if s.Properties != nil {
		out.Properties = make(map[string]any, len(s.Properties))
		for k, v := range s.Properties {
			out.Properties[k] = v
		}
	}
	if s.Required != nil {
		out.Required = append([]string(nil), s.Required...)
	}
	if s.Extra != nil {
		out.Extra = make(map[string]any, len(s.Extra))
		for k, v := range s.Extra {
			out.Extra[k] = v
		}
	}

	return out
}
