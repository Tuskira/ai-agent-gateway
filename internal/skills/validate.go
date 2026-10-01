// Package skills implements the validation, hashing and frontmatter
// rules shared by every caller that writes to the skills & commands
// registry (pkg/store.SkillStore): the API handlers
// (internal/api/handlers.Skills), the config-driven seed (SeedSkills),
// and any future caller. It is pure -- no store or HTTP dependency beyond
// pkg/store's plain SkillFile/CommandArgument value types -- so it can be
// unit tested without a database and reused unchanged by every writer.
// internal/dataplane/orchestrator additionally reuses its frontmatter
// helpers and RenderTemplate on the read side, to serve a skill's content
// and render a command's template natively.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Limits shared by every file-shape rule below.
const (
	MinFiles = 1
	MaxFiles = 20

	MaxSkillMDBytes = 64 * 1024  // SKILL.md itself
	MaxFileBytes    = 256 * 1024 // any one file
	MaxTotalBytes   = 512 * 1024 // every file combined

	MaxDescriptionLen = 1024
	MaxArguments      = 10
	MaxArgDescLen     = 256

	// SkillMDPath is the one file every version must include, at the
	// registry root.
	SkillMDPath = "SKILL.md"
)

// namePattern is a skill/command's registry name: lowercase, digits,
// dot/underscore/hyphen, 1-64 characters, starting with an alphanumeric.
// It must also not contain "__" (reserved for the
// "<connector>__<tool>" MCP naming scheme Phase 2 introduces) and must
// not be the reserved name "gateway" (the connector slug Phase 2
// reserves for the gateway's own native tools).
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// reservedNames may never be registered as a skill/command name.
var reservedNames = map[string]bool{
	"gateway": true,
}

// ValidateName enforces the skill/command name shape shared by every
// caller (API create, seed).
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("name %q must match %s", name, namePattern.String())
	}
	if strings.Contains(name, "__") {
		return fmt.Errorf("name %q must not contain \"__\"", name)
	}
	if reservedNames[name] {
		return fmt.Errorf("name %q is reserved", name)
	}
	return nil
}

// filePathPattern is the shape every file's path must match: no leading
// "/", no ".." segments (enforced separately for a clearer message), no
// trailing "/".
var filePathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// allowedExtensions is the file-extension allow-list, checked
// case-sensitively (the exact casing the contract lists).
var allowedExtensions = map[string]bool{
	".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true,
	".csv": true, ".xml": true, ".toml": true,
}

// secretPatterns are scanned against every file's raw content. A match
// anywhere in a file rejects the whole write -- see ValidateFiles.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gk_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
}

// HashFile returns the lowercase hex SHA-256 digest and byte length of
// content, the two values every stored/returned SkillFile carries
// alongside Path/Content.
func HashFile(content string) (sha256hex string, size int) {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:]), len(content)
}

// ValidateFiles checks files (each already carrying Path and Content;
// SHA256/Size are computed and filled in here, overwriting whatever the
// caller passed) against every registry file rule: count, SKILL.md
// present at the root, path shape, extension allow-list, valid UTF-8, no
// NUL bytes, per-file and total size caps, and a secret-content scan. It
// returns the completed slice (safe to persist) or the first violation
// as a plain error whose message is exactly what should reach the caller
// as a 400 validation_error.
func ValidateFiles(files []store.SkillFile) ([]store.SkillFile, error) {
	if len(files) < MinFiles || len(files) > MaxFiles {
		return nil, fmt.Errorf("files: between %d and %d files are required", MinFiles, MaxFiles)
	}

	seen := make(map[string]bool, len(files))
	hasSkillMD := false
	total := 0
	out := make([]store.SkillFile, len(files))

	for i, f := range files {
		if err := validateFilePath(f.Path); err != nil {
			return nil, fmt.Errorf("files[%d]: %w", i, err)
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("files[%d]: duplicate path %q", i, f.Path)
		}
		seen[f.Path] = true
		if f.Path == SkillMDPath {
			hasSkillMD = true
		}

		if !utf8.ValidString(f.Content) {
			return nil, fmt.Errorf("files[%d] (%s): content is not valid UTF-8", i, f.Path)
		}
		if strings.ContainsRune(f.Content, 0) {
			return nil, fmt.Errorf("files[%d] (%s): content must not contain NUL bytes", i, f.Path)
		}

		sha, size := HashFile(f.Content)
		maxBytes := MaxFileBytes
		if f.Path == SkillMDPath {
			maxBytes = MaxSkillMDBytes
		}
		if size > maxBytes {
			return nil, fmt.Errorf("files[%d] (%s): %d bytes exceeds the %d byte limit", i, f.Path, size, maxBytes)
		}
		total += size

		for _, pat := range secretPatterns {
			if pat.MatchString(f.Content) {
				return nil, fmt.Errorf("secret-like content detected in %s", f.Path)
			}
		}

		out[i] = store.SkillFile{Path: f.Path, Content: f.Content, SHA256: sha, Size: size}
	}

	if !hasSkillMD {
		return nil, fmt.Errorf("files: %s is required at the root", SkillMDPath)
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("files: total size %d bytes exceeds the %d byte limit", total, MaxTotalBytes)
	}

	return out, nil
}

// validateFilePath enforces the path shape: filePathPattern, no ".."
// segment, no leading/trailing "/", and one of the allowed extensions.
func validateFilePath(p string) error {
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("path %q must not start or end with \"/\"", p)
	}
	if !filePathPattern.MatchString(p) {
		return fmt.Errorf("path %q must match %s", p, filePathPattern.String())
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q must not contain \"..\"", p)
		}
	}
	ext := strings.ToLower(path.Ext(p))
	if !allowedExtensions[ext] {
		return fmt.Errorf("path %q has a disallowed extension %q", p, ext)
	}
	return nil
}

// FindFile returns the content of the file at p, or ok=false if files
// has none. Used to locate SKILL.md.
func FindFile(files []store.SkillFile, p string) (content string, ok bool) {
	for _, f := range files {
		if f.Path == p {
			return f.Content, true
		}
	}
	return "", false
}

// ValidateArguments checks a command's Arguments list: at most
// MaxArguments entries, each with a valid name, a bounded description,
// and no duplicate names.
func ValidateArguments(args []store.CommandArgument) error {
	if len(args) > MaxArguments {
		return fmt.Errorf("arguments: at most %d are allowed", MaxArguments)
	}
	seen := make(map[string]bool, len(args))
	for i, a := range args {
		if err := validateArgName(a.Name); err != nil {
			return fmt.Errorf("arguments[%d]: %w", i, err)
		}
		if seen[a.Name] {
			return fmt.Errorf("arguments[%d]: duplicate argument name %q", i, a.Name)
		}
		seen[a.Name] = true
		if len([]rune(a.Description)) > MaxArgDescLen {
			return fmt.Errorf("arguments[%d]: description must be at most %d characters", i, MaxArgDescLen)
		}
	}
	return nil
}

var argNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func validateArgName(name string) error {
	if !argNamePattern.MatchString(name) {
		return fmt.Errorf("name %q must match %s", name, argNamePattern.String())
	}
	return nil
}

// placeholderPattern matches a command template's {{name}} placeholders.
var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// ValidatePlaceholders checks that every {{name}} placeholder in body
// (a command's template -- SKILL.md's content after the frontmatter
// block) names a declared argument. It does not require every declared
// argument to be used.
func ValidatePlaceholders(body string, args []store.CommandArgument) error {
	declared := make(map[string]bool, len(args))
	for _, a := range args {
		declared[a.Name] = true
	}
	var undeclared []string
	seen := make(map[string]bool)
	for _, m := range placeholderPattern.FindAllStringSubmatch(body, -1) {
		name := m[1]
		if !declared[name] && !seen[name] {
			seen[name] = true
			undeclared = append(undeclared, name)
		}
	}
	if len(undeclared) == 0 {
		return nil
	}
	sort.Strings(undeclared)
	return fmt.Errorf("template references undeclared argument(s): %s", strings.Join(undeclared, ", "))
}

// RenderTemplate substitutes every {{name}} placeholder in body with
// args[name] (empty string when the caller didn't supply an optional
// one), using the exact same placeholder syntax ValidatePlaceholders
// checks against. Called by the MCP data plane
// (internal/dataplane/orchestrator) when a native command's prompts/get
// renders its template -- ValidatePlaceholders already guarantees every
// placeholder here names a declared argument (required or not), so a
// caller that validated the template at write time never sees one
// survive unsubstituted.
func RenderTemplate(body string, args map[string]string) string {
	return placeholderPattern.ReplaceAllStringFunc(body, func(m string) string {
		sub := placeholderPattern.FindStringSubmatch(m)
		return args[sub[1]]
	})
}
