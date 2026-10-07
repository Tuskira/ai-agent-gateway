package turn

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// awsKey is a distinct access key id for i, shaped as the rule wants.
func awsKey(i int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	b := []byte(join("AKIA", "Z3MFKR7QW2LXB5TN"))
	for j := len(b) - 1; i > 0; j-- {
		b[j] = alphabet[i%len(alphabet)]
		i /= len(alphabet)
	}
	return string(b)
}

// heapAfterGC is the live heap once the collector has run.
func heapAfterGC() uint64 {
	var ms runtime.MemStats
	for range 2 {
		runtime.GC()
	}
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// The scan cache keeps the values it found, not the texts they were found
// in: 200 tool outputs of 1 MiB, each with one secret, retain a few KB
// once the texts themselves are gone.
func TestScanCacheDoesNotRetainScannedText(t *testing.T) {
	withScanCache(t, true)
	secretDetector()
	n, size := 200, 1<<20
	if slowdown > 1 {
		n = 20 // the race detector: same check, fewer texts
	}
	filler := strings.Repeat("lorem ipsum dolor sit amet ", size/27+1)
	before := heapAfterGC()
	for i := range n {
		text := fmt.Sprintf("%d %s AWS_ACCESS_KEY_ID=%s\n", i, filler[:size-64], awsKey(i))
		ms, err := scanText(context.Background(), text, true)
		if err != nil || len(ms) == 0 {
			t.Fatalf("text %d: found %v, %v", i, ms, err)
		}
	}
	after := heapAfterGC()
	scanCache.Lock()
	entries := len(scanCache.m)
	scanCache.Unlock()
	if entries != n {
		t.Fatalf("cache holds %d entries, want %d", entries, n)
	}
	grew := int64(after) - int64(before)
	t.Logf("heap retained by %d cached scans of %d MiB texts: %.1f MiB", n, size>>20, float64(grew)/(1<<20))
	if grew > 8<<20 {
		t.Errorf("heap grew %.1f MiB: the cache keeps the scanned texts alive", float64(grew)/(1<<20))
	}
}

// The cache is bounded by the bytes of what it keeps, not only by its
// entries: past the budget it drops entries until it is at half, and the
// bytes it counts are those of the entries it holds.
func TestScanCacheByteBudget(t *testing.T) {
	withScanCache(t, true)
	secretDetector()
	prev := scanCacheBytes.Load()
	t.Cleanup(func() { scanCacheBytes.Store(prev) })
	const budget = 16 << 10
	SetScanCacheBytes(budget)
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 64)
	maxBytes, maxEntries := int64(0), 0
	for i := range 400 {
		text := fmt.Sprintf("%d %s AWS_ACCESS_KEY_ID=%s\n", i, filler, awsKey(i))
		if _, err := scanText(context.Background(), text, true); err != nil {
			t.Fatal(err)
		}
		scanCache.Lock()
		var sum int64
		for _, ms := range scanCache.m {
			sum += entryBytes(ms)
		}
		if sum != scanCache.bytes {
			t.Fatalf("after %d scans: counted %d bytes, entries take %d", i+1, scanCache.bytes, sum)
		}
		maxBytes, maxEntries = max(maxBytes, scanCache.bytes), max(maxEntries, len(scanCache.m))
		scanCache.Unlock()
	}
	t.Logf("400 entries of about %d bytes: at most %d bytes in %d entries (budget %d)", entryBytes([]secretMatch{{"aws-access-token", awsKey(1)}}), maxBytes, maxEntries, budget)
	if maxBytes > budget {
		t.Errorf("cache held %d bytes, past its %d budget", maxBytes, budget)
	}
	if maxEntries >= 400 || maxEntries == 0 {
		t.Errorf("cache held at most %d entries: nothing evicted, or nothing kept", maxEntries)
	}
	// A cached text is still answered from the cache, the same values.
	text := fmt.Sprintf("%d %s AWS_ACCESS_KEY_ID=%s\n", 399, filler, awsKey(399))
	ms, _ := scanText(context.Background(), text, true)
	if len(ms) != 1 || ms[0].secret != awsKey(399) {
		t.Errorf("rescan = %+v", ms)
	}
	// 0 or less is the default.
	SetScanCacheBytes(0)
	if got := scanCacheBytes.Load(); got != DefaultScanCacheBytes {
		t.Errorf("SetScanCacheBytes(0): budget %d", got)
	}
}
