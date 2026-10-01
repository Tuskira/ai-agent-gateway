package skills

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"weather-lookup", false},
		{"weather_lookup.v2", false},
		{"a", false},
		{strings.Repeat("a", 64), false},
		{strings.Repeat("a", 65), true}, // too long
		{"", true},
		{"Weather", true},         // uppercase
		{"-weather", true},        // must start alnum
		{"weather lookup", true},  // space
		{"weather__lookup", true}, // double underscore reserved
		{"gateway", true},         // reserved name
		{"gateway.v2", false},     // not an exact reserved-name match
	}
	for _, c := range cases {
		err := ValidateName(c.name)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateName(%q) error = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func validSkillMD(name string) string {
	return "---\nname: " + name + "\ndescription: A test skill.\n---\nBody text.\n"
}

func baseFiles(name string) []store.SkillFile {
	return []store.SkillFile{{Path: "SKILL.md", Content: validSkillMD(name)}}
}

func TestValidateFiles_Valid(t *testing.T) {
	files := baseFiles("demo")
	files = append(files, store.SkillFile{Path: "reference.md", Content: "extra reference material"})

	out, err := ValidateFiles(files)
	if err != nil {
		t.Fatalf("ValidateFiles: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("ValidateFiles() = %d files, want 2", len(out))
	}
	for _, f := range out {
		if f.SHA256 == "" || f.Size == 0 {
			t.Errorf("file %q: SHA256/Size not computed: %+v", f.Path, f)
		}
	}
}

func TestValidateFiles_Rules(t *testing.T) {
	cases := []struct {
		name  string
		files []store.SkillFile
	}{
		{"too few files", []store.SkillFile{}},
		{"too many files", func() []store.SkillFile {
			fs := baseFiles("demo")
			for i := 0; i < MaxFiles; i++ {
				fs = append(fs, store.SkillFile{Path: strings.Repeat("f", i) + ".txt", Content: "x"})
			}
			return fs
		}()},
		{"missing SKILL.md", []store.SkillFile{{Path: "notes.md", Content: "x"}}},
		{"duplicate path", []store.SkillFile{
			{Path: "SKILL.md", Content: validSkillMD("demo")},
			{Path: "SKILL.md", Content: validSkillMD("demo")},
		}},
		{"leading slash", []store.SkillFile{
			{Path: "SKILL.md", Content: validSkillMD("demo")},
			{Path: "/etc/passwd.txt", Content: "x"},
		}},
		{"trailing slash", []store.SkillFile{
			{Path: "SKILL.md", Content: validSkillMD("demo")},
			{Path: "notes/", Content: "x"},
		}},
		{"dot-dot segment", []store.SkillFile{
			{Path: "SKILL.md", Content: validSkillMD("demo")},
			{Path: "../escape.txt", Content: "x"},
		}},
		{"disallowed extension", []store.SkillFile{
			{Path: "SKILL.md", Content: validSkillMD("demo")},
			{Path: "script.sh", Content: "x"},
		}},
		{"invalid UTF-8", []store.SkillFile{
			{Path: "SKILL.md", Content: string([]byte{0xff, 0xfe, 0xfd})},
		}},
		{"NUL byte", []store.SkillFile{
			{Path: "SKILL.md", Content: "---\nname: demo\ndescription: x\n---\n\x00"},
		}},
		{"SKILL.md too large", []store.SkillFile{
			{Path: "SKILL.md", Content: validSkillMD("demo") + strings.Repeat("a", MaxSkillMDBytes)},
		}},
		{"file too large", []store.SkillFile{
			{Path: "SKILL.md", Content: validSkillMD("demo")},
			{Path: "big.txt", Content: strings.Repeat("a", MaxFileBytes+1)},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ValidateFiles(c.files); err == nil {
				t.Errorf("ValidateFiles(%s) error = nil, want an error", c.name)
			}
		})
	}
}

func TestValidateFiles_TotalSizeLimit(t *testing.T) {
	files := baseFiles("demo")
	// Two files, each under the per-file cap, but together over the
	// total cap.
	half := MaxTotalBytes/2 + 1024
	files = append(files,
		store.SkillFile{Path: "a.txt", Content: strings.Repeat("a", half)},
		store.SkillFile{Path: "b.txt", Content: strings.Repeat("b", half)},
	)
	if _, err := ValidateFiles(files); err == nil {
		t.Error("ValidateFiles(total over limit) error = nil, want an error")
	}
}

// secretSamples pairs each contract-mandated secret pattern with one
// string it must match, so a regression in any single pattern surfaces
// as a specific test failure.
var secretSamples = []struct {
	name   string
	sample string
}{
	{"gk_ token", "gk_" + strings.Repeat("a", 24)},
	{"AWS access key", "AKIA" + strings.Repeat("A", 16)},
	{"PEM private key", "-----BEGIN RSA PRIVATE KEY-----"},
	{"GitHub PAT", "ghp_" + strings.Repeat("a", 36)},
	{"Slack token", "xoxb-1234567890-abcdefghij"},
	{"Anthropic key", "sk-ant-" + strings.Repeat("a", 24)},
	{"generic sk- key", "sk-" + strings.Repeat("a", 32)},
	{"Google API key", "AIza" + strings.Repeat("A", 35)},
}

func TestValidateFiles_SecretScan(t *testing.T) {
	for _, c := range secretSamples {
		t.Run(c.name, func(t *testing.T) {
			files := []store.SkillFile{
				{Path: "SKILL.md", Content: validSkillMD("demo")},
				{Path: "notes.txt", Content: "here is a secret: " + c.sample},
			}
			_, err := ValidateFiles(files)
			if err == nil {
				t.Fatalf("ValidateFiles(%s) error = nil, want secret-like content rejection", c.name)
			}
			if !strings.Contains(err.Error(), "secret-like content detected in notes.txt") {
				t.Errorf("ValidateFiles(%s) error = %q, want it to name notes.txt", c.name, err.Error())
			}
		})
	}
}

func TestValidateArguments(t *testing.T) {
	valid := []store.CommandArgument{{Name: "target", Description: "the host to scan", Required: true}}
	if err := ValidateArguments(valid); err != nil {
		t.Errorf("ValidateArguments(valid) error = %v", err)
	}

	tooMany := make([]store.CommandArgument, MaxArguments+1)
	for i := range tooMany {
		tooMany[i] = store.CommandArgument{Name: "a" + strconv.Itoa(i)}
	}
	if err := ValidateArguments(tooMany); err == nil {
		t.Error("ValidateArguments(too many) error = nil, want an error")
	}

	badName := []store.CommandArgument{{Name: "Target"}}
	if err := ValidateArguments(badName); err == nil {
		t.Error("ValidateArguments(bad name) error = nil, want an error")
	}

	dup := []store.CommandArgument{{Name: "target"}, {Name: "target"}}
	if err := ValidateArguments(dup); err == nil {
		t.Error("ValidateArguments(duplicate name) error = nil, want an error")
	}

	longDesc := []store.CommandArgument{{Name: "target", Description: strings.Repeat("a", MaxArgDescLen+1)}}
	if err := ValidateArguments(longDesc); err == nil {
		t.Error("ValidateArguments(long description) error = nil, want an error")
	}
}

func TestValidatePlaceholders(t *testing.T) {
	args := []store.CommandArgument{{Name: "target"}, {Name: "port"}}

	if err := ValidatePlaceholders("scan {{target}} on {{port}}", args); err != nil {
		t.Errorf("ValidatePlaceholders(declared) error = %v", err)
	}
	if err := ValidatePlaceholders("scan {{target}}, no port needed", args); err != nil {
		t.Errorf("ValidatePlaceholders(unused declared arg) error = %v", err)
	}
	if err := ValidatePlaceholders("scan {{target}} for {{cve}}", args); err == nil {
		t.Error("ValidatePlaceholders(undeclared) error = nil, want an error")
	}
}

func TestRenderTemplate(t *testing.T) {
	got := RenderTemplate("scan {{target}} on {{port}}", map[string]string{"target": "10.0.0.1", "port": "443"})
	if want := "scan 10.0.0.1 on 443"; got != want {
		t.Errorf("RenderTemplate() = %q, want %q", got, want)
	}
}

func TestRenderTemplate_UnsuppliedOptionalArgumentBecomesEmpty(t *testing.T) {
	got := RenderTemplate("scan {{target}} for {{cve}}", map[string]string{"target": "10.0.0.1"})
	if want := "scan 10.0.0.1 for "; got != want {
		t.Errorf("RenderTemplate() = %q, want %q", got, want)
	}
}

func TestRenderTemplate_NoPlaceholdersIsUnchanged(t *testing.T) {
	got := RenderTemplate("no placeholders here", nil)
	if want := "no placeholders here"; got != want {
		t.Errorf("RenderTemplate() = %q, want %q", got, want)
	}
}
