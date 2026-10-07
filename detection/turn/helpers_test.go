package turn

import (
	"context"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/detection/wire/conv"
)

// prepareState is st prepared as PrepareRequest / PrepareResponse would
// once the body is extracted.
func prepareState(st *State, stage Stage) PreparedTurn {
	hits, err := st.scrub(context.Background(), stage, nil, false)
	if err != nil {
		panic(err) // no deadline
	}
	return newTurn(stage, st, hits)
}

// resetScanCache empties scanCache, so a run is timed cold.
func resetScanCache() {
	scanCache.Lock()
	clear(scanCache.m)
	scanCache.bytes = 0
	scanCache.Unlock()
}

// withScanCache turns the scan cache on (or off) for one test or benchmark,
// emptied before and after, and restores the setting.
func withScanCache(tb testing.TB, on bool) {
	tb.Helper()
	prev := scanCacheOn.Load()
	scanCacheOn.Store(on)
	resetScanCache()
	tb.Cleanup(func() { scanCacheOn.Store(prev); resetScanCache() })
}

// prepareRequest is PrepareRequestContext with no deadline.
func prepareRequest(body []byte) PreparedTurn {
	return PrepareRequestContext(context.Background(), body)
}

// prepareResponse is PrepareResponseContext with no deadline.
func prepareResponse(reqBody, respBody []byte) (PreparedTurn, bool) {
	return PrepareResponseContext(context.Background(), reqBody, respBody)
}

// requestState is the clipped state of a request body, as prepare reads it
// before the secret scan.
func requestState(body []byte) (State, bool) {
	s, ok := extractRequest(newReader(context.Background()), body)
	s.clip()
	return s, ok
}

// requestStateFromConversation is requestState over a canonical
// conversation.
func requestStateFromConversation(c *conv.Conversation) (State, bool) {
	s, ok := extractConversation(newReader(context.Background()), c)
	s.clip()
	return s, ok
}
