package skills

import (
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestSplitFrontmatter(t *testing.T) {
	fm, body, err := SplitFrontmatter("---\nname: demo\ndescription: x\n---\nHello.\n")
	if err != nil {
		t.Fatalf("SplitFrontmatter: %v", err)
	}
	if !strings.Contains(fm, "name: demo") {
		t.Errorf("frontmatter = %q, want it to contain name: demo", fm)
	}
	if strings.TrimSpace(body) != "Hello." {
		t.Errorf("body = %q, want %q", body, "Hello.")
	}

	if _, _, err := SplitFrontmatter("no frontmatter here"); err == nil {
		t.Error("SplitFrontmatter(no delimiter) error = nil, want an error")
	}
	if _, _, err := SplitFrontmatter("---\nname: demo\nunclosed"); err == nil {
		t.Error("SplitFrontmatter(unclosed) error = nil, want an error")
	}
}

func TestParseFrontmatter_Valid(t *testing.T) {
	md := "---\nname: demo\ndescription: A demo skill.\nlicense: MIT\n---\nBody.\n"
	fm, err := ParseFrontmatter(md, "demo")
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fm["name"] != "demo" || fm["license"] != "MIT" {
		t.Errorf("ParseFrontmatter() = %+v", fm)
	}
	if DescriptionFrom(fm) != "A demo skill." {
		t.Errorf("DescriptionFrom() = %q", DescriptionFrom(fm))
	}
}

func TestParseFrontmatter_HooksRejected(t *testing.T) {
	md := "---\nname: demo\ndescription: x\nhooks:\n  pre: echo hi\n---\nBody.\n"
	if _, err := ParseFrontmatter(md, "demo"); err == nil {
		t.Error("ParseFrontmatter(hooks) error = nil, want rejection")
	} else if !strings.Contains(err.Error(), "hooks") {
		t.Errorf("ParseFrontmatter(hooks) error = %q, want it to mention hooks", err.Error())
	}
}

// rejectedKeys covers every allowed-list boundary case worth a dedicated
// assertion: one key from each family that IS allowed (control), and a
// representative sample of keys that must be rejected as unknown.
func TestParseFrontmatter_AllowedKeys(t *testing.T) {
	allowed := []string{
		"name", "description", "license", "compatibility", "metadata", "allowed-tools",
		"user-invocable", "disable-model-invocation", "context", "agent", "background",
		"model", "effort", "paths", "shell", "arguments", "argument-hint", "when_to_use",
	}
	for _, key := range allowed {
		if !allowedFrontmatterKeys[key] {
			t.Errorf("allowedFrontmatterKeys missing contract key %q", key)
		}
	}

	rejected := []string{"hooks", "run", "exec", "command", "script", "env", "permissions", "unknown_field"}
	for _, key := range rejected {
		t.Run(key, func(t *testing.T) {
			md := "---\nname: demo\ndescription: x\n" + key + ": y\n---\nBody.\n"
			if _, err := ParseFrontmatter(md, "demo"); err == nil {
				t.Errorf("ParseFrontmatter(key=%s) error = nil, want rejection", key)
			}
		})
	}
}

func TestParseFrontmatter_NameMustMatch(t *testing.T) {
	md := "---\nname: other\ndescription: x\n---\nBody.\n"
	if _, err := ParseFrontmatter(md, "demo"); err == nil {
		t.Error("ParseFrontmatter(name mismatch) error = nil, want an error")
	}
}

func TestParseFrontmatter_DescriptionBounds(t *testing.T) {
	empty := "---\nname: demo\ndescription: \"\"\n---\nBody.\n"
	if _, err := ParseFrontmatter(empty, "demo"); err == nil {
		t.Error("ParseFrontmatter(empty description) error = nil, want an error")
	}

	tooLong := "---\nname: demo\ndescription: \"" + strings.Repeat("a", MaxDescriptionLen+1) + "\"\n---\nBody.\n"
	if _, err := ParseFrontmatter(tooLong, "demo"); err == nil {
		t.Error("ParseFrontmatter(description too long) error = nil, want an error")
	}
}

func TestParseFrontmatter_InvalidYAML(t *testing.T) {
	md := "---\nname: [unterminated\n---\nBody.\n"
	if _, err := ParseFrontmatter(md, "demo"); err == nil {
		t.Error("ParseFrontmatter(invalid YAML) error = nil, want an error")
	}
}

func TestFrontmatterKind(t *testing.T) {
	if got := FrontmatterKind(map[string]any{}); got != "skill" {
		t.Errorf("FrontmatterKind(no metadata) = %q, want skill", got)
	}
	if got := FrontmatterKind(map[string]any{"metadata": map[string]any{"kind": "command"}}); got != "command" {
		t.Errorf("FrontmatterKind(command) = %q, want command", got)
	}
	if got := FrontmatterKind(map[string]any{"metadata": map[string]any{"kind": "bogus"}}); got != "skill" {
		t.Errorf("FrontmatterKind(bogus) = %q, want skill (default)", got)
	}
}

func TestFindSkillMD(t *testing.T) {
	files := []store.SkillFile{{Path: "SKILL.md", Content: "---\nname: demo\ndescription: x\n---\nBody.\n"}}
	content, err := FindSkillMD(files)
	if err != nil {
		t.Fatalf("FindSkillMD: %v", err)
	}
	if !strings.Contains(content, "name: demo") {
		t.Errorf("FindSkillMD() = %q", content)
	}

	if _, err := FindSkillMD([]store.SkillFile{{Path: "notes.md", Content: "x"}}); err == nil {
		t.Error("FindSkillMD(missing) error = nil, want an error")
	}
}
