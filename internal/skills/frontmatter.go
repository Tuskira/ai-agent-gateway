package skills

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// allowedFrontmatterKeys is the full set of keys SKILL.md's YAML front
// matter may declare: the portable Agent Skills fields plus the Claude
// Code fields the contract lists. "hooks" is deliberately absent -- see
// ParseFrontmatter.
var allowedFrontmatterKeys = map[string]bool{
	// Portable Agent Skills fields.
	"name": true, "description": true, "license": true,
	"compatibility": true, "metadata": true, "allowed-tools": true,
	// Claude Code fields.
	"user-invocable": true, "disable-model-invocation": true, "context": true,
	"agent": true, "background": true, "model": true, "effort": true,
	"paths": true, "shell": true, "arguments": true, "argument-hint": true,
	"when_to_use": true,
}

// SplitFrontmatter splits a SKILL.md's raw content into its YAML front
// matter block (the text between the two "---" delimiter lines,
// exclusive) and the body that follows. content must start with a "---"
// line; the block must be closed by a second "---" line.
func SplitFrontmatter(content string) (frontmatterYAML, body string, err error) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", "", fmt.Errorf("%s must start with YAML frontmatter delimited by \"---\"", SkillMDPath)
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), nil
		}
	}
	return "", "", fmt.Errorf("%s frontmatter is not closed with a second \"---\"", SkillMDPath)
}

// ParseFrontmatter parses SKILL.md's raw content into its frontmatter
// map, validating it against the allowed-key list (an unknown key is a
// 400; "hooks" is explicitly rejected since it would let a skill run
// shell commands) and checking the two required fields: name (must equal
// skillName) and description (1..MaxDescriptionLen characters). The
// returned map is the raw parsed YAML (JSON-marshalable), suitable for
// pkg/store.Skill.Frontmatter.
func ParseFrontmatter(skillMD, skillName string) (map[string]any, error) {
	fmYAML, _, err := SplitFrontmatter(skillMD)
	if err != nil {
		return nil, err
	}

	var raw map[string]any
	if err := yaml.Unmarshal([]byte(fmYAML), &raw); err != nil {
		return nil, fmt.Errorf("%s frontmatter is not valid YAML: %w", SkillMDPath, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}

	if _, ok := raw["hooks"]; ok {
		return nil, fmt.Errorf("frontmatter key \"hooks\" is not allowed")
	}
	for key := range raw {
		if !allowedFrontmatterKeys[key] {
			return nil, fmt.Errorf("frontmatter key %q is not allowed", key)
		}
	}

	name, _ := raw["name"].(string)
	if name != skillName {
		return nil, fmt.Errorf("frontmatter name %q must equal the skill name %q", name, skillName)
	}

	description, _ := raw["description"].(string)
	descLen := len([]rune(description))
	if descLen < 1 || descLen > MaxDescriptionLen {
		return nil, fmt.Errorf("frontmatter description must be between 1 and %d characters", MaxDescriptionLen)
	}

	return raw, nil
}

// DescriptionFrom returns fm["description"] as a string, or "" if absent
// or not a string.
func DescriptionFrom(fm map[string]any) string {
	d, _ := fm["description"].(string)
	return d
}

// FrontmatterKind returns the seed-only metadata.kind override ("skill"
// or "command"), defaulting to "skill" when absent or invalid. Used by
// SeedSkills, which has no separate "kind" field in its directory format
// (see docs, "Seed").
func FrontmatterKind(fm map[string]any) string {
	meta, _ := fm["metadata"].(map[string]any)
	if kind, _ := meta["kind"].(string); kind == "command" {
		return "command"
	}
	return "skill"
}

// FindSkillMD returns the content of the SKILL.md file in files, or an
// error if absent. Callers that already ran ValidateFiles never see this
// error in practice (SKILL.md presence is one of its rules); this exists
// for store-layer callers (SkillStore.AddVersion) that receive
// already-validated files and just need to re-derive frontmatter/
// description without redoing full validation.
func FindSkillMD(files []store.SkillFile) (string, error) {
	content, ok := FindFile(files, SkillMDPath)
	if !ok {
		return "", fmt.Errorf("files: %s is required at the root", SkillMDPath)
	}
	return content, nil
}
