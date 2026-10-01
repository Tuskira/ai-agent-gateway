package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DecodeYAMLStrict decodes YAML into dst and rejects any key dst has no field
// for, so a typo ("require_profle") fails loudly instead of being silently
// ignored. An empty document is accepted and leaves dst untouched. name is the
// file name used in error messages.
func DecodeYAMLStrict(data []byte, name string, dst any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	err := dec.Decode(dst)
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return fmt.Errorf("config %s: %w", name, err)
	}
	var root yaml.Node
	_ = yaml.Unmarshal(data, &root)
	msgs := make([]string, 0, len(te.Errors))
	for _, m := range te.Errors {
		msgs = append(msgs, readableYAMLError(m, &root))
	}
	return fmt.Errorf("config %s: %s", name, strings.Join(msgs, "; "))
}

var reUnknownField = regexp.MustCompile(`^line (\d+): field (\S+) not found in type `)

// readableYAMLError turns yaml.v3's `line 3: field x not found in type
// config.MCP` into `unknown key "mcp.x" at line 3`, resolving the dotted path
// from the parsed node tree. Other messages pass through unchanged.
func readableYAMLError(msg string, root *yaml.Node) string {
	m := reUnknownField.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	line, _ := strconv.Atoi(m[1])
	key := m[2]
	if path, ok := findKeyPath(root, nil, line, key); ok {
		key = strings.Join(path, ".")
	}
	return fmt.Sprintf("unknown key %q at line %d", key, line)
}

func findKeyPath(n *yaml.Node, path []string, line int, key string) ([]string, bool) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for i, c := range n.Content {
			p := path
			if n.Kind == yaml.SequenceNode {
				p = append(append([]string{}, path...), fmt.Sprintf("[%d]", i))
			}
			if r, ok := findKeyPath(c, p, line, key); ok {
				return r, true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := append(append([]string{}, path...), k.Value)
			if k.Line == line && k.Value == key {
				return p, true
			}
			if r, ok := findKeyPath(v, p, line, key); ok {
				return r, true
			}
		}
	}
	return nil, false
}

// envNotFromConfig are GATEWAY_* variables read outside the config struct
// (CLI credentials, compose plumbing); they are never "unknown".
var envNotFromConfig = map[string]bool{
	"GATEWAY_MASTER_KEY": true, "GATEWAY_ADMIN_KEY": true, "GATEWAY_KEY": true,
	"GATEWAY_URL": true, "GATEWAY_IMAGE": true, "GATEWAY_CONTROL_BASE": true,
}

// UnknownEnvVars returns the GATEWAY_* environment variables that map to no
// config key (typos such as GATEWAY_MCP_REQUIRE_PROFLE). The environment can
// legitimately carry unrelated GATEWAY_* names, so callers log these as a
// warning rather than failing.
func UnknownEnvVars(cfg *Config) []string {
	known := map[string]bool{}
	collectEnvNames(reflect.ValueOf(cfg).Elem(), "GATEWAY", known)
	if cfg.SecretStore.MasterKeyEnv != "" {
		known[cfg.SecretStore.MasterKeyEnv] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "GATEWAY_") || known[name] || envNotFromConfig[name] ||
			strings.HasPrefix(name, "GATEWAY_TEST_") || strings.HasPrefix(name, "GATEWAY_E2E_") ||
			strings.HasPrefix(name, "GATEWAY_COMPOSE") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func collectEnvNames(v reflect.Value, prefix string, into map[string]bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		fv := v.Field(i)
		name := prefix + "_" + strings.ToUpper(yamlTagName(t.Field(i)))
		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			collectEnvNames(fv, name, into)
			continue
		}
		into[name] = true
	}
}
