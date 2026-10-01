package postgres

import "encoding/json"

// marshalJSONB encodes a JSONB column value, treating a nil map as an
// empty object so columns declared NOT NULL DEFAULT '{}' round-trip
// cleanly.
func marshalJSONB(v map[string]any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

// unmarshalJSONB decodes a JSONB column value into a map[string]any,
// treating empty/NULL bytes as an empty object.
func unmarshalJSONB(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
