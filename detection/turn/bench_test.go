package turn

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// prose is n bytes of text that looks like what agents send: words that
// trip gitleaks' keyword prefilter (key, token, secret, password, api ...)
// so every keyword rule runs its regex, but no secret.
func prose(n int) string {
	words := strings.Fields(`the api key for the service token lives in config yaml and the password
	rotation job reads secret values from vault before auth github slack stripe aws azure gcp
	client id bearer jwt private access credential deploy build test run fix bug module func
	return error value string int map slice channel goroutine context deadline http request`)
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString(words[(i*7+i/13)%len(words)])
		if i%17 == 16 {
			fmt.Fprintf(&b, " = %d;\n", i)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()[:n]
}

func userBody(text string) []byte {
	b, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": text}}})
	return b
}

// toolResultsBody is a turn returning n tool results of size bytes each.
func toolResultsBody(n, size int) []byte {
	var uses, results []map[string]any
	for i := range n {
		id := fmt.Sprintf("tu_%d", i)
		uses = append(uses, map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"path": "/src/" + id}})
		results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": prose(size)})
	}
	b, _ := json.Marshal(map[string]any{"messages": []map[string]any{
		{"role": "user", "content": "read the sources"},
		{"role": "assistant", "content": uses},
		{"role": "user", "content": results},
	}})
	return b
}

// Each op runs cold (scanCache emptied first, not timed) unless the case
// is warm: the same body again, as an agent's next call resends its
// history.
func BenchmarkPrepareRequest(b *testing.B) {
	secretDetector() // built once, not timed
	for _, c := range []struct {
		name string
		body []byte
		warm bool
	}{
		{"user_text_10KB", userBody(prose(10 << 10)), false},
		{"user_text_1MiB", userBody(prose(1 << 20)), false},
		{"user_text_10MiB", userBody(prose(10 << 20)), false},
		{"tool_results_50x200KB", toolResultsBody(50, 200<<10), false},
		{"tool_result_20MiB_key_block", keyBlockBody(20 << 20), false},
		{"history_40x20KB", historyBody(40, 20<<10), false},
		{"history_40x20KB_warm", historyBody(40, 20<<10), true},
	} {
		b.Run(c.name, func(b *testing.B) {
			withScanCache(b, true)
			b.SetBytes(int64(len(c.body)))
			for b.Loop() {
				if !c.warm {
					b.StopTimer()
					resetScanCache()
					b.StartTimer()
				}
				prepareRequest(c.body)
			}
		})
	}
}

func BenchmarkPrepareResponse(b *testing.B) {
	secretDetector()
	resp, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": prose(1 << 20)}}})
	withScanCache(b, true)
	b.SetBytes(int64(len(resp)))
	for b.Loop() {
		b.StopTimer()
		resetScanCache()
		b.StartTimer()
		prepareResponse([]byte(claudeCodeTurn), resp)
	}
}

// keyBlockBody is a turn returning one tool output of about size bytes
// shaped like a private key block (BEGIN, size bytes of keyword-dense text,
// END): before the widening bound, the scan took the whole of it.
func keyBlockBody(size int) []byte {
	filler := strings.Repeat("key: token password secret auth api x ", size/38)
	b, _ := json.Marshal(map[string]any{"messages": []map[string]any{
		{"role": "user", "content": "read it"},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"p": "x"}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1",
			"content": join("-----BEGIN RSA ", "PRIVATE KEY-----\n") + filler + join("\n-----END RSA ", "PRIVATE KEY-----")}}},
	}})
	return b
}

// historyBody is a conversation of n earlier turns, each a tool call and a
// distinct result of size bytes, then a new user message: every string of
// the history is scanned for values to remove.
func historyBody(n, size int) []byte {
	var msgs []map[string]any
	for i := range n {
		id := fmt.Sprintf("tu_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "user", "content": fmt.Sprintf("step %d", i)},
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"path": "/src/" + id}}}},
			map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": fmt.Sprintf("file %d\n", i) + prose(size)}}},
			map[string]any{"role": "assistant", "content": "read"})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "now fix the bug"})
	b, _ := json.Marshal(map[string]any{"messages": msgs})
	return b
}
