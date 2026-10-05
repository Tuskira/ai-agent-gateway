package turn

import (
	"context"
	"testing"
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
