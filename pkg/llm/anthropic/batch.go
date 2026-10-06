package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// BatchReaderName is the registry name of BatchReader.
const BatchReaderName = "anthropic_batch"

func init() { llm.RegisterBatchReader(BatchReader{}) }

// BatchReader is the llm.BatchReader for Message Batches creation
// (POST /v1/messages/batches): {"requests": [{"custom_id", "params"}]},
// each params a Messages request body, read as the Messages Reader reads
// one.
//
// It is as strict as the API: requests must be a non-empty list, each item
// holds exactly custom_id and params, a custom_id is 1 to 64 letters,
// digits, "-" or "_" and is used once in the batch, and the top level holds
// requests alone.
type BatchReader struct{}

func (BatchReader) Name() string { return BatchReaderName }

// DecodeBatch reads a Message Batches creation body.
func (BatchReader) DecodeBatch(body []byte) ([]llm.BatchItem, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	items, err := readBatch(body)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return items, nil
}

func readBatch(body []byte) ([]llm.BatchItem, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	var requests []map[string]json.RawMessage
	if raw, ok := top["requests"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &requests); err != nil {
			return nil, fmt.Errorf("requests: %v", err)
		}
	}
	delete(top, "requests")
	if k := firstKey(top); k != "" {
		return nil, fmt.Errorf("%s: unknown field", k)
	}
	if len(requests) == 0 {
		return nil, errors.New("requests: must hold at least one request")
	}
	items := make([]llm.BatchItem, 0, len(requests))
	seen := make(map[string]int, len(requests))
	for i, r := range requests {
		if r == nil {
			return nil, fmt.Errorf("requests.%d: must be an object", i)
		}
		var id string
		if err := json.Unmarshal(r["custom_id"], &id); err != nil {
			return nil, fmt.Errorf("requests.%d.custom_id: must be a string", i)
		}
		if !validCustomID(id) {
			return nil, fmt.Errorf("requests.%d.custom_id: must be 1 to 64 letters, digits, - or _", i)
		}
		if first, dup := seen[id]; dup {
			return nil, fmt.Errorf("requests.%d.custom_id: %q repeats requests.%d", i, id, first)
		}
		seen[id] = i
		params, ok := r["params"]
		if !ok || isNull(params) {
			return nil, fmt.Errorf("requests.%d.params: required", i)
		}
		delete(r, "custom_id")
		delete(r, "params")
		if k := firstKey(r); k != "" {
			return nil, fmt.Errorf("requests.%d.%s: unknown field", i, k)
		}
		req, err := readRequest(params)
		if err != nil {
			return nil, fmt.Errorf("requests.%d.params: %v", i, err)
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

// validCustomID is the API's custom_id rule: ^[a-zA-Z0-9_-]{1,64}$.
func validCustomID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}
