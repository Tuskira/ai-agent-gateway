// Package configs embeds the shipped sample config, so built-in defaults
// that live there (the MCP catalog) are not duplicated in Go.
package configs

import _ "embed"

// DefaultYAML is configs/base/default.yml.
//
//go:embed base/default.yml
var DefaultYAML []byte
