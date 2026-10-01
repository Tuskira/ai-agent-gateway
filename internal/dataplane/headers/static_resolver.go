package headers

import (
	"context"
	"fmt"
	"strings"
)

// staticResolver is the "static" built-in HeaderResolver: it returns
// cfg["value"] verbatim. Config: {"type": "static", "value": "..."}.
type staticResolver struct{}

func (staticResolver) Type() string { return "static" }

func (staticResolver) Validate(cfg map[string]any) error {
	v, _ := cfg["value"].(string)
	if v == "" {
		return fmt.Errorf("static: \"value\" is required")
	}
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("static: \"value\" must not contain a line break")
	}
	return nil
}

func (staticResolver) Resolve(_ context.Context, cfg map[string]any) (string, error) {
	v, _ := cfg["value"].(string)
	return v, nil
}
