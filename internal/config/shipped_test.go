package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// topLevelKeys are the section names a gateway config may start with.
func topLevelKeys() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		keys[yamlTagName(t.Field(i))] = true
	}
	return keys
}

// looksLikeGatewayConfig reports whether doc is a mapping whose every
// top-level key is a gateway config section (so compose files, k8s manifests
// and the OTel collector config are never mistaken for one).
func looksLikeGatewayConfig(doc []byte) bool {
	var m map[string]any
	if yaml.Unmarshal(doc, &m) != nil || len(m) == 0 {
		return false
	}
	keys := topLevelKeys()
	for k := range m {
		if !keys[k] {
			return false
		}
	}
	return true
}

var reFence = regexp.MustCompile("(?s)```ya?ml\n(.*?)```")

// TestShippedConfigsLoadStrictly walks the repo for every gateway config the
// project ships (configs/, deploy/, examples/, ConfigMap-embedded configs and
// copy-pasteable yaml fences in the docs) and decodes each with strict keys,
// so a renamed or removed key cannot leave a shipped example that the gateway
// now refuses to start with.
func TestShippedConfigsLoadStrictly(t *testing.T) {
	root := repoRoot(t)
	var found int
	check := func(name string, doc []byte, full bool) {
		t.Helper()
		if !looksLikeGatewayConfig(doc) {
			return
		}
		found++
		t.Run(name, func(t *testing.T) {
			if err := DecodeYAMLStrict(doc, name, &Config{}); err != nil {
				t.Fatalf("shipped config is rejected: %v", err)
			}
			if !full {
				return // docs snippets may be fragments; strict keys is the contract
			}
			f := filepath.Join(t.TempDir(), "c.yml")
			if err := os.WriteFile(f, doc, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(f); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}

	skip := map[string]bool{"node_modules": true, ".git": true, ".claude": true, "dist": true}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		ext := filepath.Ext(p)
		if ext != ".yml" && ext != ".yaml" && ext != ".md" {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if ext == ".md" {
			for i, m := range reFence.FindAllSubmatch(data, -1) {
				check(rel+"#fence"+string(rune('0'+i%10)), m[1], false)
			}
			return nil
		}
		check(rel, data, true)
		// ConfigMaps carry the config as a data value.
		var cm struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
		}
		if yaml.Unmarshal(data, &cm) == nil && cm.Kind == "ConfigMap" {
			for k, v := range cm.Data {
				check(rel+"["+k+"]", []byte(v), true)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if found < 3 {
		t.Fatalf("only %d shipped configs found; the walker is broken", found)
	}
}

// TestShippedEnvVarsMapToConfigKeys fails when a deploy/ or examples/ manifest
// sets a GATEWAY_* variable that no config key reads (a typo or a removed key).
func TestShippedEnvVarsMapToConfigKeys(t *testing.T) {
	root := repoRoot(t)
	known := map[string]bool{}
	collectEnvNames(reflect.ValueOf(&Config{}).Elem(), "GATEWAY", known)
	re := regexp.MustCompile(`\bGATEWAY_[A-Z0-9_]*[A-Z0-9]\b`)
	for _, dir := range []string{"deploy", "examples"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || (!strings.HasSuffix(p, ".yml") && !strings.HasSuffix(p, ".yaml")) {
				return nil
			}
			data, _ := os.ReadFile(p)
			for _, name := range re.FindAllString(string(data), -1) {
				if known[name] || envNotFromConfig[name] || strings.HasPrefix(name, "GATEWAY_COMPOSE") ||
					strings.HasPrefix(name, "GATEWAY_TEST_") {
					continue
				}
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s sets %s, which maps to no config key", rel, name)
			}
			return nil
		})
	}
}

func TestStrictRejectsUnknownKeys(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"nested typo", "service:\n  name: x\nmcp:\n  require_profle: true\n", `unknown key "mcp.require_profle" at line 4`},
		{"top level", "bogus_top: 1\n", `unknown key "bogus_top" at line 1`},
		{"deep", "llm_proxy:\n  capture:\n    body_store:\n      tpye: s3\n", `unknown key "llm_proxy.capture.body_store.tpye" at line 4`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := DecodeYAMLStrict([]byte(c.doc), "gw.yml", &Config{})
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "config gw.yml:") {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
	for _, empty := range []string{"", "# only a comment\n", "---\n"} {
		if err := DecodeYAMLStrict([]byte(empty), "gw.yml", &Config{}); err != nil {
			t.Errorf("empty document %q rejected: %v", empty, err)
		}
	}
}

func TestUnknownEnvVars(t *testing.T) {
	t.Setenv("GATEWAY_MCP_REQUIRE_PROFLE", "true")
	t.Setenv("GATEWAY_MCP_REQUIRE_PROFILE", "true")
	t.Setenv("GATEWAY_ADMIN_KEY", "x")
	got := UnknownEnvVars(Default())
	if len(got) != 1 || got[0] != "GATEWAY_MCP_REQUIRE_PROFLE" {
		t.Fatalf("UnknownEnvVars = %v", got)
	}
}
