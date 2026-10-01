// Command notices generates THIRD_PARTY_NOTICES for the gateway binary and
// enforces a license allow list while doing it. Run via `make notices`
// (see ../../Makefile), which is also what .goreleaser.yaml's `before.hooks`
// and the CI "go" job (.github/workflows/ci.yml) call.
//
// It does two things in one pass over `go list -deps` for the given
// package (default ./cmd/gateway, i.e. everything that actually ships in
// the released binary):
//
//  1. Classifies every third-party Go module's license (via
//     `go run github.com/google/go-licenses/v2@<pinnedGoLicensesVersion>
//     report`, pinned below) and fails the build if any module resolves to
//     a license outside allowedLicenses.
//  2. Writes a plain-text THIRD_PARTY_NOTICES file: one entry per module
//     with its name, version, license, the module's own full license
//     text, and its NOTICE file's text when it has one -- plus a closing
//     entry for the Go standard library, which isn't a module at all so
//     go list/go-licenses never see it.
//
// THIRD_PARTY_NOTICES is generated, not committed (see ../../.gitignore) --
// it's produced fresh at release/build time, same as the binary itself.
//
// This lives in its own Go module (./go.mod) specifically so that
// go-licenses's own dependency tree (k8s.io/klog, google/licenseclassifier,
// ...) never has to appear in the gateway's go.mod/go.sum. `go run
// module@version` already gives that isolation for go-licenses itself;
// putting *this* command in a sibling module gives the same isolation to
// its own (today: zero) dependencies, and keeps it out of the gateway
// module's `go build ./...` / `go vet ./...` / `golangci-lint run ./...`
// entirely, since those don't recurse into a nested module.
package main

import (
	"bytes"
	"embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// pinnedGoLicensesVersion is the exact go-licenses/v2 release this command
// shells out to (via `go run`, so it's never added to any go.mod). Bump
// deliberately -- check https://github.com/google/go-licenses/releases
// first, since this tool's CSV parsing (parseGoLicensesReport) depends on
// its default `report` output staying "import path,source URL,license".
const pinnedGoLicensesVersion = "v2.0.1"

// allowedLicenses is the full set of SPDX identifiers go-licenses is
// allowed to resolve a module to. Every one of this project's actual Go
// dependencies is MIT, BSD-2-Clause, BSD-3-Clause, or Apache-2.0 today;
// ISC is included because it's an equally permissive, equally common
// license for a transitive dependency to carry. Keep this list short and
// deliberate -- it is the thing standing between a future `go get` of a
// copyleft-licensed dependency and a release shipping it unnoticed.
var allowedLicenses = map[string]bool{
	"MIT":          true,
	"BSD-2-Clause": true,
	"BSD-3-Clause": true,
	"Apache-2.0":   true,
	"ISC":          true,
}

// overrides lists modules whose correct SPDX license id this command
// could not get from go-licenses' classifier, verified by hand instead by
// reading the module's own license file. Keep this list as short as
// possible and justify every entry with a comment: it is manual,
// unchecked-by-tooling input to a license allow-list gate.
var overrides = map[string]string{
	// github.com/segmentio/asm@v1.2.1's LICENSE file text is "MIT No
	// Attribution" (SPDX MIT-0): a public-domain-equivalent variant of
	// MIT that drops the attribution requirement, strictly more
	// permissive than plain MIT. go-licenses v2.0.1's classifier does
	// not have that exact variant's text in its corpus and reports
	// "no license found" for every package under this module (verified
	// by running `go run github.com/google/go-licenses/v2@v2.0.1 report`
	// directly and reading
	// $(go env GOMODCACHE)/github.com/segmentio/asm@v1.2.1/LICENSE).
	"github.com/segmentio/asm": "MIT",
}

//go:embed golang_stdlib_LICENSE.txt
var golangStdlibLicenseFS embed.FS

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run() error {
	pkg := flag.String("pkg", "./cmd/gateway", "Go package to compute the dependency closure of (as passed to `go list -deps` and `go-licenses report`)")
	repoRoot := flag.String("repo-root", ".", "gateway module root (where go.mod lives); -pkg is resolved relative to this")
	out := flag.String("out", "THIRD_PARTY_NOTICES", "output file path, relative to -repo-root unless absolute")
	flag.Parse()

	root, err := filepath.Abs(*repoRoot)
	if err != nil {
		return fmt.Errorf("resolving -repo-root: %w", err)
	}

	pkgs, err := goListDeps(root, *pkg)
	if err != nil {
		return fmt.Errorf("go list -deps %s: %w", *pkg, err)
	}

	modules := groupByModule(pkgs)
	if len(modules) == 0 {
		return fmt.Errorf("no third-party modules found in the dependency closure of %s -- that's almost certainly this tool's bug, not reality", *pkg)
	}

	rows, err := goLicensesReport(root, *pkg)
	if err != nil {
		return fmt.Errorf("go-licenses report: %w", err)
	}

	resolved, failures := resolveLicenses(modules, rows)
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("%d module(s) failed license resolution/allow-list check:\n  %s",
			len(failures), strings.Join(failures, "\n  "))
	}

	goVersion, err := goToolVersion(root)
	if err != nil {
		return fmt.Errorf("go version: %w", err)
	}

	content, err := renderNotices(modules, rows, resolved, goVersion)
	if err != nil {
		return fmt.Errorf("rendering THIRD_PARTY_NOTICES: %w", err)
	}

	outPath := *out
	if !filepath.IsAbs(outPath) {
		outPath = filepath.Join(root, outPath)
	}
	if err := os.WriteFile(outPath, content, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	counts := make(map[string]int)
	for _, lics := range resolved {
		for _, lic := range lics {
			counts[lic]++
		}
	}
	fmt.Printf("notices: %d third-party module(s) OK (", len(modules))
	licNames := make([]string, 0, len(counts))
	for lic := range counts {
		licNames = append(licNames, lic)
	}
	sort.Strings(licNames)
	parts := make([]string, 0, len(licNames))
	for _, lic := range licNames {
		parts = append(parts, fmt.Sprintf("%s:%d", lic, counts[lic]))
	}
	fmt.Printf("%s); wrote %s\n", strings.Join(parts, ", "), outPath)
	return nil
}

// pkgInfo is the subset of `go list -json` per-package fields this tool
// needs.
type pkgInfo struct {
	ImportPath string
	Standard   bool
	Module     *moduleInfo
}

type moduleInfo struct {
	Path    string
	Version string
	Dir     string
	Main    bool
}

// goListDeps runs `go list -deps -json <pkg>` from root and decodes the
// resulting stream of back-to-back JSON objects (not a JSON array -- that's
// `go list`'s actual -json output format for multiple packages).
func goListDeps(root, pkg string) ([]pkgInfo, error) {
	cmd := exec.Command("go", "list", "-deps", "-json", pkg)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, stderr.String())
	}

	dec := json.NewDecoder(&stdout)
	var pkgs []pkgInfo
	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// groupByModule reduces a package list to the distinct third-party modules
// in it (dropping the standard library and the gateway's own main module),
// keyed by module path.
func groupByModule(pkgs []pkgInfo) map[string]moduleInfo {
	modules := make(map[string]moduleInfo)
	for _, p := range pkgs {
		if p.Standard || p.Module == nil || p.Module.Main {
			continue
		}
		modules[p.Module.Path] = *p.Module
	}
	return modules
}

// licenseRow is one row of go-licenses' CSV report output: the package
// whose license this is, the source URL go-licenses found the license text
// at, and the SPDX id it classified that text as ("Unknown" if it
// couldn't).
type licenseRow struct {
	Pkg     string
	URL     string
	License string
}

// goLicensesReport runs `go run github.com/google/go-licenses/v2@<pinned>
// report <pkg>` and parses its CSV output (import path, source URL, SPDX
// license id). Rows go-licenses could not classify come back with License
// "Unknown" (its own report format, not an error) -- resolveLicenses is
// what decides whether that's fatal for a given module.
func goLicensesReport(root, pkg string) ([]licenseRow, error) {
	module := fmt.Sprintf("github.com/google/go-licenses/v2@%s", pinnedGoLicensesVersion)
	cmd := exec.Command("go", "run", module, "report", pkg)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// go-licenses exits 0 even when individual packages are
	// unclassifiable (it logs "no license found" to stderr, which we
	// surface below for a human but don't treat as fatal by itself --
	// resolveLicenses is the actual gate). A non-zero exit here means
	// go-licenses itself failed to run at all (bad package path, module
	// download failure, ...), which *is* fatal.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, stderr.String())
	}
	if stderr.Len() > 0 {
		fmt.Fprint(os.Stderr, "notices: go-licenses warnings:\n", stderr.String())
	}

	r := csv.NewReader(&stdout)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing go-licenses CSV output: %w", err)
	}

	rows := make([]licenseRow, 0, len(records))
	for _, rec := range records {
		if len(rec) < 3 {
			continue
		}
		rows = append(rows, licenseRow{Pkg: rec[0], URL: rec[1], License: rec[2]})
	}
	return rows, nil
}

// rowsForModule returns every non-"Unknown" row belonging to modPath (its
// own package, or any of its sub-packages).
func rowsForModule(rows []licenseRow, modPath string) []licenseRow {
	var out []licenseRow
	for _, r := range rows {
		if r.License == "Unknown" {
			continue
		}
		if r.Pkg == modPath || strings.HasPrefix(r.Pkg, modPath+"/") {
			out = append(out, r)
		}
	}
	return out
}

// resolveLicenses reduces each module's per-package license classifications
// to the set of distinct SPDX ids it carries and checks every one against
// allowedLicenses. It is normal, not an error, for a module to carry more
// than one permissive license at once -- e.g. github.com/klauspost/compress
// bundles MIT, Apache-2.0, and BSD-3-Clause texts for different vendored
// components in one module; renderNotices includes all of their texts. A
// module is a failure if: go-licenses classified nothing for it and
// there's no override, or any license it carries (classified or
// overridden) isn't in allowedLicenses.
func resolveLicenses(modules map[string]moduleInfo, rows []licenseRow) (resolved map[string][]string, failures []string) {
	resolved = make(map[string][]string, len(modules))

	for modPath := range modules {
		matched := rowsForModule(rows, modPath)

		var lics []string
		if len(matched) == 0 {
			o, ok := overrides[modPath]
			if !ok {
				failures = append(failures, fmt.Sprintf("%s: go-licenses could not classify any package in this module, and there's no entry in overrides (main.go) -- read its LICENSE file by hand and either fix the classifier input or add a justified override", modPath))
				continue
			}
			lics = []string{o}
		} else {
			seen := make(map[string]bool, len(matched))
			for _, r := range matched {
				seen[r.License] = true
			}
			for l := range seen {
				lics = append(lics, l)
			}
			sort.Strings(lics)
		}

		bad := false
		for _, l := range lics {
			if !allowedLicenses[l] {
				failures = append(failures, fmt.Sprintf("%s: license %q is not in the allow list", modPath, l))
				bad = true
			}
		}
		if bad {
			continue
		}
		resolved[modPath] = lics
	}

	return resolved, failures
}

// licenseFileNames / noticeFileNames are tried in order against a module's
// directory; the first match wins. Every third-party module in this
// project's dependency closure has one of these at its root today
// (verified by hand when this tool was written), but if a future
// dependency doesn't, findFile returning "" turns into an empty license
// text in the notices file rather than a silent wrong answer -- renderNotices
// flags that loudly.
var (
	licenseFileNames = []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt", "COPYING", "COPYING.md"}
	noticeFileNames  = []string{"NOTICE", "NOTICE.md", "NOTICE.txt"}
)

func findFile(dir string, candidates []string) string {
	for _, name := range candidates {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// resolveLicenseFile turns a go-licenses report row's source URL into a
// local path under modDir, if one exists. The URL always has the shape
// https://<host>/<org>/<repo>/blob/<ref>/<path-within-repo>, but <ref>
// itself can contain slashes for a multi-module monorepo (e.g.
// "config/v1.33.5" for the github.com/aws/aws-sdk-go-v2/config submodule,
// whose own module root -- modDir -- is already that "config/" subtree).
// So rather than parse out <ref> precisely, try the path after "/blob/" as
// a whole, then progressively drop one leading segment at a time (assuming
// more of the front is part of <ref>) until something under modDir
// actually exists.
func resolveLicenseFile(modDir, rawURL string) string {
	const marker = "/blob/"
	i := strings.Index(rawURL, marker)
	if i < 0 {
		return ""
	}
	segments := strings.Split(rawURL[i+len(marker):], "/")
	for start := 0; start < len(segments); start++ {
		candidate := filepath.Join(append([]string{modDir}, segments[start:]...)...)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
	}
	return ""
}

func readFileOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n") + "\n"
}

// goToolVersion returns e.g. "go1.27.1" for the toolchain that just ran
// go-licenses/go list above, for the standard-library entry's version
// field -- a real release build regenerates this file with its own CI Go
// toolchain (go.mod's go-version-file pin), so this is always the version
// that actually built the binary it ships alongside, not a guess.
func goToolVersion(root string) (string, error) {
	cmd := exec.Command("go", "env", "GOVERSION")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var multiBlankLines = regexp.MustCompile(`\n{3,}`)

// licenseBlock is one distinct license text to print for a module: the
// SPDX id(s) it covers (more than one when, e.g., a single combined
// LICENSE file bundles more than one license, as
// github.com/klauspost/compress's does) and the text itself.
type licenseBlock struct {
	Labels []string
	Text   string
}

// moduleBlocks finds every distinct license text a module carries: the
// module's own rows (by SPDX id) are grouped by the actual local file
// go-licenses found each one in (via resolveLicenseFile), so a sub-package
// under a different license than the module root -- e.g.
// github.com/aws/aws-sdk-go-v2/internal/sync/singleflight's own
// BSD-3-Clause LICENSE, vendored inside the otherwise Apache-2.0
// github.com/aws/aws-sdk-go-v2 module -- gets its own block, while rows
// that resolve to the same file (klauspost/compress's MIT/Apache-2.0/
// BSD-3-Clause rows all point at one combined LICENSE) collapse into one.
// If go-licenses classified nothing for this module (the override path),
// it returns a single block for the module's root license file labeled
// with the manual override.
func moduleBlocks(modPath string, mod moduleInfo, rows []licenseRow) ([]licenseBlock, error) {
	matched := rowsForModule(rows, modPath)

	if len(matched) == 0 {
		lic, ok := overrides[modPath]
		if !ok {
			// resolveLicenses would already have failed the build
			// before renderNotices runs; this is defensive.
			return nil, fmt.Errorf("no classified license and no override for %s", modPath)
		}
		fp := findFile(mod.Dir, licenseFileNames)
		text := readFileOrEmpty(fp)
		if text == "" {
			return nil, fmt.Errorf("%s: manually overridden to %q but no license file was found under %s", modPath, lic, mod.Dir)
		}
		return []licenseBlock{{Labels: []string{lic + " (manual override, see tools/notices/main.go)"}, Text: text}}, nil
	}

	byFile := make(map[string]map[string]bool)
	var order []string
	for _, r := range matched {
		fp := resolveLicenseFile(mod.Dir, r.URL)
		if fp == "" {
			fp = findFile(mod.Dir, licenseFileNames)
		}
		if fp == "" {
			continue
		}
		if byFile[fp] == nil {
			byFile[fp] = make(map[string]bool)
			order = append(order, fp)
		}
		byFile[fp][r.License] = true
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("%s: classified but no license file could be located under %s", modPath, mod.Dir)
	}

	blocks := make([]licenseBlock, 0, len(order))
	for _, fp := range order {
		labels := make([]string, 0, len(byFile[fp]))
		for l := range byFile[fp] {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		text := readFileOrEmpty(fp)
		if text == "" {
			return nil, fmt.Errorf("%s: could not read resolved license file %s", modPath, fp)
		}
		blocks = append(blocks, licenseBlock{Labels: labels, Text: text})
	}
	return blocks, nil
}

// renderNotices builds the full THIRD_PARTY_NOTICES text: a fixed header,
// one entry per resolved module in path order, and a closing entry for the
// Go standard library.
func renderNotices(modules map[string]moduleInfo, rows []licenseRow, resolved map[string][]string, goVersion string) ([]byte, error) {
	var b strings.Builder

	b.WriteString(noticesHeader)

	paths := make([]string, 0, len(modules))
	for p := range modules {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		mod := modules[path]
		lics := resolved[path]

		fmt.Fprintf(&b, "================================================================================\n")
		fmt.Fprintf(&b, "%s %s\n", path, mod.Version)
		fmt.Fprintf(&b, "License: %s\n", strings.Join(lics, ", "))
		b.WriteString("================================================================================\n\n")

		blocks, err := moduleBlocks(path, mod, rows)
		if err != nil {
			return nil, err
		}
		for i, blk := range blocks {
			if len(blocks) > 1 {
				fmt.Fprintf(&b, "[%s]\n\n", strings.Join(blk.Labels, ", "))
			}
			b.WriteString(blk.Text)
			if i < len(blocks)-1 {
				b.WriteString("\n")
			}
		}

		if noticeText := readFileOrEmpty(findFile(mod.Dir, noticeFileNames)); noticeText != "" {
			b.WriteString("\n--- NOTICE ---\n\n")
			b.WriteString(noticeText)
		}
		b.WriteString("\n")
	}

	golangLicense, err := golangStdlibLicenseFS.ReadFile("golang_stdlib_LICENSE.txt")
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&b, "================================================================================\n")
	fmt.Fprintf(&b, "The Go Programming Language (standard library) %s\n", goVersion)
	b.WriteString("License: BSD-3-Clause\n")
	b.WriteString("================================================================================\n\n")
	b.Write(golangLicense)
	b.WriteString("\n")

	return []byte(multiBlankLines.ReplaceAllString(b.String(), "\n\n")), nil
}

const noticesHeader = `THIRD-PARTY NOTICES

This file is generated by ` + "`make notices`" + ` (tools/notices) at build/release
time -- see .goreleaser.yaml and .github/workflows/ci.yml -- and is not
committed to source control. It lists every third-party Go module compiled
into the tusk-ai-secured-gateway binary (the dependency closure of
./cmd/gateway), each with its own license text and, where the module ships
one, its NOTICE file.

The gateway binary also embeds a prebuilt React console (web/); that
console's own third-party JavaScript dependencies (and their licenses) are
listed separately, at build time, in web/dist/licenses.txt -- served by the
running gateway at /licenses.txt -- not in this file.

This file's base container image (gcr.io/distroless/static-debian12, see
Dockerfile) also contains a small number of Debian packages (CA
certificates and the files distroless/static needs for a static Go binary
to run as a non-root user). Debian does not ship a single combined NOTICE
file; see https://www.debian.org/distrib/packages and
https://snapshot.debian.org for every package's own source and license.

tusk-ai-secured-gateway itself is Apache-2.0; see LICENSE and NOTICE in the
repository root.

`
