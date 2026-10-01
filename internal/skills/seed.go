package skills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// SeedSkills walks seedDir and upserts every immediate subdirectory
// containing a SKILL.md as a PLATFORM row (tenant_id NULL) in skillStore,
// by name -- the same role llmplane.SeedModels plays for the model
// registry, sourced from a directory of skill bundles instead of inline
// config.
//
// For each subdirectory: every regular file directly inside it (not
// walked recursively) becomes one SkillFile, with its path relative to
// the subdirectory (e.g. "SKILL.md", "reference.md"); SKILL.md's
// frontmatter provides the skill's name (must equal the subdirectory
// name) and description, and "kind" is FrontmatterKind(frontmatter's
// metadata.kind), "skill" by default; "metadata.arguments", when kind is
// "command", provides the command's argument list.
//
// A platform row of that name that doesn't exist yet is created; one
// that exists is left alone unless the new files differ from its current
// latest version (compared by path set + SHA256 digest), in which case a
// new version is added. Rows seedDir does not mention are left alone --
// the directory is a seed, not the source of truth, so an operator can
// still add platform rows another way (the API) without the next restart
// deleting them. Idempotent and safe for every replica to run at boot.
// Returns how many rows were created and how many got a new version.
func SeedSkills(ctx context.Context, skillStore store.SkillStore, seedDir string, logger *slog.Logger) (created, updated int, err error) {
	if seedDir == "" {
		return 0, 0, nil
	}

	entries, err := os.ReadDir(seedDir)
	if err != nil {
		return 0, 0, fmt.Errorf("read skills seed dir %q: %w", seedDir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		dir := filepath.Join(seedDir, name)
		files, err := loadSeedFiles(dir)
		if err != nil {
			return created, updated, fmt.Errorf("seed skill %q: %w", name, err)
		}
		if files == nil {
			continue // no SKILL.md in this subdirectory: not a skill bundle
		}

		validated, err := ValidateFiles(files)
		if err != nil {
			return created, updated, fmt.Errorf("seed skill %q: %w", name, err)
		}
		skillMD, err := FindSkillMD(validated)
		if err != nil {
			return created, updated, fmt.Errorf("seed skill %q: %w", name, err)
		}
		fm, err := ParseFrontmatter(skillMD, name)
		if err != nil {
			return created, updated, fmt.Errorf("seed skill %q: %w", name, err)
		}
		kind := FrontmatterKind(fm)

		var args []store.CommandArgument
		if kind == "command" {
			args, err = seedArguments(fm)
			if err != nil {
				return created, updated, fmt.Errorf("seed skill %q: %w", name, err)
			}
			body, ok := frontmatterBody(skillMD)
			if ok {
				if err := ValidatePlaceholders(body, args); err != nil {
					return created, updated, fmt.Errorf("seed skill %q: %w", name, err)
				}
			}
		}

		existing, err := skillStore.GetByName(ctx, "", name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			sk := &store.Skill{
				Name: name, Kind: kind, Description: DescriptionFrom(fm), Frontmatter: fm,
				Arguments: args, Enabled: true, Metadata: map[string]any{"seed": "config"},
			}
			err := skillStore.Create(ctx, sk, validated, "seed")
			if errors.Is(err, store.ErrConflict) {
				continue // another replica booting at the same time created it
			}
			if err != nil {
				return created, updated, fmt.Errorf("seed skill %q: create: %w", name, err)
			}
			created++
		case err != nil:
			return created, updated, fmt.Errorf("seed skill %q: look up: %w", name, err)
		default:
			current, err := skillStore.GetVersion(ctx, existing.ID, existing.LatestVersion)
			if err != nil {
				return created, updated, fmt.Errorf("seed skill %q: load current version: %w", name, err)
			}
			if filesEqual(current.Files, validated) {
				continue // unchanged
			}
			if _, err := skillStore.AddVersion(ctx, "", existing.ID, validated, "seed"); err != nil {
				return created, updated, fmt.Errorf("seed skill %q: add version: %w", name, err)
			}
			if !existing.Enabled || !argumentsEqual(existing.Arguments, args) {
				existing.Arguments = args
				existing.Enabled = true
				if err := skillStore.Update(ctx, existing); err != nil {
					return created, updated, fmt.Errorf("seed skill %q: update arguments: %w", name, err)
				}
			}
			updated++
		}
	}

	if logger != nil && (created > 0 || updated > 0) {
		logger.Info("skills registry seeded from directory", "dir", seedDir, "created", created, "updated", updated)
	}
	return created, updated, nil
}

// loadSeedFiles reads every regular file directly inside dir (no
// recursion) into store.SkillFile{Path, Content} pairs, path relative to
// dir. Returns nil, nil (not an error) when dir has no SKILL.md -- the
// caller's signal to skip a subdirectory that isn't a skill bundle.
func loadSeedFiles(dir string) ([]store.SkillFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}

	hasSkillMD := false
	files := make([]store.SkillFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if e.Name() == SkillMDPath {
			hasSkillMD = true
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", filepath.Join(dir, e.Name()), err)
		}
		files = append(files, store.SkillFile{Path: e.Name(), Content: string(content)})
	}
	if !hasSkillMD {
		return nil, nil
	}
	return files, nil
}

// seedArguments reads metadata.arguments from a command's frontmatter:
// a list of {name, description?, required?} objects.
func seedArguments(fm map[string]any) ([]store.CommandArgument, error) {
	meta, _ := fm["metadata"].(map[string]any)
	raw, ok := meta["arguments"].([]any)
	if !ok {
		return nil, nil
	}
	args := make([]store.CommandArgument, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("metadata.arguments[%d] must be an object", i)
		}
		name, _ := m["name"].(string)
		desc, _ := m["description"].(string)
		required, _ := m["required"].(bool)
		args = append(args, store.CommandArgument{Name: name, Description: desc, Required: required})
	}
	if err := ValidateArguments(args); err != nil {
		return nil, err
	}
	return args, nil
}

// frontmatterBody returns the body text after SKILL.md's frontmatter
// block, ok=false if it can't be split (already validated by the time
// this is called, so that should not happen in practice).
func frontmatterBody(skillMD string) (string, bool) {
	_, body, err := SplitFrontmatter(skillMD)
	if err != nil {
		return "", false
	}
	return body, true
}

// filesEqual reports whether two file sets are identical: same paths
// (any order) each with the same SHA256 digest.
func filesEqual(a, b []store.SkillFile) bool {
	if len(a) != len(b) {
		return false
	}
	byPath := make(map[string]string, len(a))
	for _, f := range a {
		byPath[f.Path] = f.SHA256
	}
	for _, f := range b {
		sha, ok := byPath[f.Path]
		if !ok || sha != f.SHA256 {
			return false
		}
	}
	return true
}

// argumentsEqual reports whether two CommandArgument lists are
// equivalent (order-independent, by name/description/required).
func argumentsEqual(a, b []store.CommandArgument) bool {
	if len(a) != len(b) {
		return false
	}
	byName := make(map[string]store.CommandArgument, len(a))
	for _, arg := range a {
		byName[arg.Name] = arg
	}
	for _, arg := range b {
		other, ok := byName[arg.Name]
		if !ok || other.Description != arg.Description || other.Required != arg.Required {
			return false
		}
	}
	return true
}
