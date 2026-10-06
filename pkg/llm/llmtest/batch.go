package llmtest

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// batchCase is one batch reader golden,
// testdata/readers/<batch reader name>/<case>.json.
type batchCase struct {
	// Request is the client's batch creation body.
	Request json.RawMessage `json:"request"`
	Expect  struct {
		// Items are the batch's items in order: {custom_id, request}, each
		// request the neutral llm.Request.
		Items []struct {
			CustomID string          `json:"custom_id"`
			Request  json.RawMessage `json:"request"`
		} `json:"items"`
		// RequestError is instead a substring of the *llm.RequestError the
		// body must fail with; BatchFile instead that it fails with
		// llm.ErrBatchFile.
		RequestError string `json:"request_error"`
		BatchFile    bool   `json:"batch_file"`
	} `json:"expect"`
}

// RunBatchReader checks r against batch reader goldens: every *.json case at
// the root of goldens, or, with goldens nil, the samples shipped in
// testdata/readers/<r.Name()>/. Each case's body must decode to the
// expected items (or fail as expected). It also checks a malformed body is
// a *llm.RequestError.
func RunBatchReader(t *testing.T, r llm.BatchReader, goldens fs.FS) {
	t.Run("malformed_request", func(t *testing.T) {
		_, err := r.DecodeBatch([]byte("{not json"))
		if !errors.As(err, new(*llm.RequestError)) {
			t.Errorf("err = %v, want *llm.RequestError", err)
		}
	})
	dir := "."
	if goldens == nil {
		goldens, dir = fixtures, path.Join("testdata", "readers", r.Name())
	}
	entries, err := fs.ReadDir(goldens, dir)
	if err != nil {
		t.Skipf("no batch reader goldens for %q", r.Name())
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := fs.ReadFile(goldens, path.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var c batchCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Run(strings.TrimSuffix(name, ".json"), func(t *testing.T) { runBatchCase(t, r, c) })
	}
}

func runBatchCase(t *testing.T, r llm.BatchReader, c batchCase) {
	x := c.Expect
	items, err := r.DecodeBatch(c.Request)
	switch {
	case x.BatchFile:
		if !errors.Is(err, llm.ErrBatchFile) {
			t.Fatalf("err = %v, want llm.ErrBatchFile", err)
		}
		return
	case x.RequestError != "":
		if !errors.As(err, new(*llm.RequestError)) || !strings.Contains(err.Error(), x.RequestError) {
			t.Fatalf("err = %v, want a *llm.RequestError containing %q", err, x.RequestError)
		}
		return
	case err != nil:
		t.Fatal(err)
	}
	if len(items) != len(x.Items) {
		t.Fatalf("%d items, want %d", len(items), len(x.Items))
	}
	for i, it := range items {
		if it.CustomID != x.Items[i].CustomID {
			t.Errorf("item %d custom_id = %q, want %q", i, it.CustomID, x.Items[i].CustomID)
		}
		got, _ := json.Marshal(it.Request)
		sameJSON(t, got, x.Items[i].Request)
	}
}
