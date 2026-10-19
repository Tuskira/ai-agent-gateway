package gateway

import (
	"context"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/analytics"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/sink"
)

// A nil entry in an Options slice fails Run before it loads config or
// touches the database, instead of panicking on the first request.
func TestRun_RejectsNilOptionEntries(t *testing.T) {
	for want, opts := range map[string]Options{
		"Options.Authenticators[0]": {Authenticators: []pkgauth.Authenticator{nil}},
		"Options.LogSinks[1]":       {LogSinks: []sink.LogSink{&recordingSink{}, nil}},
	} {
		// The path does not exist: reaching config.Load would fail with a
		// different error.
		opts.ConfigPath = filepath.Join(t.TempDir(), "missing.yml")
		if err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), want+" is nil") {
			t.Errorf("Run error = %v, want %q named", err, want)
		}
	}
}

// recordingSink is a LogSink that keeps what it is given and whether it
// was closed.
type recordingSink struct {
	mu     sync.Mutex
	access []*sink.AccessLog
	llm    []*sink.LLMCall
	closed bool
}

func (s *recordingSink) WriteAccess(a *sink.AccessLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = append(s.access, a)
}

func (s *recordingSink) WriteLLMCall(c *sink.LLMCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.llm = append(s.llm, c)
}

func (s *recordingSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Options.LogSinks join the built-in sinks: they get every record, they
// count as a sink (no "no log sinks" warning), and closing the gateway's
// Multi leaves them open for their owner to close.
func TestBuildSinks_ExtraSinksAreCallerOwned(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Default()
	cfg.Sinks.Stdout.Enabled = false
	cfg.Sinks.Otel.Enabled = false
	cfg.Sinks.ClickHouse.Enabled = false

	a, b := &recordingSink{}, &recordingSink{}
	res, err := buildSinks(cfg, nil, []sink.LogSink{a, b}, logger)
	if err != nil {
		t.Fatal(err)
	}
	res.Sink.WriteAccess(&sink.AccessLog{TenantID: "t", Method: "ping"})
	res.Sink.WriteLLMCall(&sink.LLMCall{TenantID: "t", RequestID: "r1"})
	if err := res.Sink.Close(); err != nil {
		t.Fatal(err)
	}
	for i, s := range []*recordingSink{a, b} {
		if len(s.access) != 1 || len(s.llm) != 1 || s.llm[0].RequestID != "r1" {
			t.Errorf("sink %d got %d access / %d llm records, want 1 / 1", i, len(s.access), len(s.llm))
		}
		if s.closed {
			t.Errorf("sink %d was closed by the gateway; the caller owns it", i)
		}
	}
	if strings.Contains(logs.String(), "no log sinks enabled") {
		t.Error("warned that no sink is enabled although two were supplied")
	}

	// Without extras, the built-in behaviour (and warning) is unchanged.
	logs.Reset()
	res, err = buildSinks(cfg, nil, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Sink.Close()
	if !strings.Contains(logs.String(), "no log sinks enabled") {
		t.Error("no warning with no sinks at all")
	}
}

// pkg/gateway sits on top of the module: no package under internal/ or
// pkg/ may import it (that would make the plugin seams depend on the whole
// gateway, and is an import cycle for every package Run itself uses).
// cmd/ and test/ are its callers.
func TestNoPackageBelowImportsGateway(t *testing.T) {
	const self = "github.com/Tuskira/tusk-ai-secured-gateway/pkg/gateway"
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range []string{"internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || path == filepath.Join(root, "pkg", "gateway") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == self || strings.HasPrefix(p, self+"/") {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %s", rel, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 100 {
		t.Fatalf("checked only %d files; is the walk rooted at the module?", checked)
	}
}

// Options.LLMCalls is the LLM Logs source when set; otherwise the built-in
// choice applies (none here: the capture store is not Postgres).
func TestLLMCalls_SuppliedReaderWins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.LLMProxy.Capture.Store = "stdout"
	supplied := suppliedCalls{}
	if got := llmCalls(supplied, cfg, nil, logger); got != supplied {
		t.Errorf("llmCalls = %v, want the supplied reader", got)
	}
	if got := llmCalls(nil, cfg, nil, logger); got != nil {
		t.Errorf("llmCalls with nothing supplied and a stdout capture store = %v, want nil", got)
	}
}

type suppliedCalls struct{ analytics.LLMCallReader }
