package orchestrator

import (
	"encoding/json"
	"fmt"
)

// decodeParams decodes a JSON-RPC params member into v. Absent params
// decode to the zero value rather than an error: initialize, tools/list
// and ping are all legitimately callable with no params at all.
func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}
