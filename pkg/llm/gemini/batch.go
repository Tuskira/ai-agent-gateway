package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// BatchName is the registry name of BatchReader.
const BatchName = "gemini_batch"

func init() { llm.RegisterBatchReader(BatchReader{}) }

// BatchReader is the llm.BatchReader for batchGenerateContent:
// {"batch": {..., "inputConfig": {"requests": {"requests": [{"request",
// "metadata"}]}}}}, each request a generateContent body, read as the Reader
// reads one. Field names are read in either spelling, as the Reader reads
// them.
//
// An item's CustomID is its metadata.key when that is a string, else its
// position in the list ("0", "1", ...); an id used twice is refused. A
// batch whose inputConfig names an uploaded file (fileName) is
// llm.ErrBatchFile. The top level holds batch alone, and inputConfig one
// of fileName and requests; the batch's other fields (displayName,
// priority, ...) are not read.
type BatchReader struct{}

func (BatchReader) Name() string { return BatchName }

// DecodeBatch reads a batchGenerateContent body.
func (BatchReader) DecodeBatch(body []byte) ([]llm.BatchItem, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	items, err := readBatch(body)
	if errors.Is(err, llm.ErrBatchFile) {
		return nil, err
	}
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return items, nil
}

func readBatch(body []byte) ([]llm.BatchItem, error) {
	top, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	batch, err := takeObject(top, "batch")
	if err != nil {
		return nil, err
	}
	if k := firstKey(top); k != "" {
		return nil, fmt.Errorf("%s: unknown field", k)
	}
	if batch == nil {
		return nil, errors.New("batch: required")
	}
	in, err := takeObject(batch, "inputConfig")
	if err != nil {
		return nil, fmt.Errorf("batch.%v", err)
	}
	if in == nil {
		return nil, errors.New("batch.inputConfig: required")
	}
	var file string
	if err := take(in, "fileName", &file); err != nil {
		return nil, fmt.Errorf("batch.inputConfig.%v", err)
	}
	inlined, err := takeObject(in, "requests")
	if err != nil {
		return nil, fmt.Errorf("batch.inputConfig.%v", err)
	}
	if k := firstKey(in); k != "" {
		return nil, fmt.Errorf("batch.inputConfig.%s: unknown field", k)
	}
	switch {
	case file != "" && inlined != nil:
		return nil, errors.New("batch.inputConfig: fileName and requests are exclusive")
	case file != "":
		return nil, llm.ErrBatchFile
	case inlined == nil:
		return nil, errors.New("batch.inputConfig: requests required")
	}
	var list []json.RawMessage
	if err := take(inlined, "requests", &list); err != nil {
		return nil, fmt.Errorf("batch.inputConfig.requests.%v", err)
	}
	if k := firstKey(inlined); k != "" {
		return nil, fmt.Errorf("batch.inputConfig.requests.%s: unknown field", k)
	}
	if len(list) == 0 {
		return nil, errors.New("batch.inputConfig.requests.requests: must hold at least one request")
	}
	items := make([]llm.BatchItem, 0, len(list))
	seen := make(map[string]int, len(list))
	for i, raw := range list {
		path := fmt.Sprintf("batch.inputConfig.requests.requests.%d", i)
		item, err := object(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		id := strconv.Itoa(i)
		if meta, ok := item["metadata"]; ok && !isNull(meta) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(meta, &m); err != nil || m == nil {
				return nil, fmt.Errorf("%s.metadata: must be an object", path)
			}
			var key string
			if k := m["key"]; !isNull(k) && json.Unmarshal(k, &key) == nil {
				id = key
			}
		}
		if first, dup := seen[id]; dup {
			return nil, fmt.Errorf("%s: id %q repeats item %d", path, id, first)
		}
		seen[id] = i
		reqRaw, ok := item["request"]
		if !ok || isNull(reqRaw) {
			return nil, fmt.Errorf("%s.request: required", path)
		}
		delete(item, "request")
		delete(item, "metadata")
		if k := firstKey(item); k != "" {
			return nil, fmt.Errorf("%s.%s: unknown field", path, k)
		}
		req, err := readRequest(reqRaw)
		if err != nil {
			return nil, fmt.Errorf("%s.request: %v", path, err)
		}
		items = append(items, llm.BatchItem{CustomID: id, Request: req})
	}
	return items, nil
}

// firstKey is m's first key in sorted order, "" when m is empty.
func firstKey(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}
