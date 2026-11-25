package sink

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// Request attribution limits for the LLM plane's X-Gateway-User and
// X-Gateway-Tags headers (and the "tags" field of an ingested llm_calls
// entry). Both are self-reported by the caller: they label a row for cost
// attribution and never affect the tenant, the API key or any limit.
const (
	// MaxUserLen bounds LLMCall.User when it comes from X-Gateway-User.
	MaxUserLen = 256
	// MaxTagKeyLen and MaxTagValueLen bound one tag. The key length is part
	// of TagKeyPattern; the value is measured after percent-decoding.
	MaxTagKeyLen   = 64
	MaxTagValueLen = 256
	// DefaultMaxTagPairs is the default cap on tags per call
	// (llm_proxy.capture.tags.max_pairs).
	DefaultMaxTagPairs = 16
	// MaxTagsHeaderLen bounds the whole X-Gateway-Tags header. A longer one
	// is cut back to its last complete pair; the rest counts as dropped.
	MaxTagsHeaderLen = 4096
)

// TagKeyPattern is the shape of a tag key: lower-case, starts with a letter,
// at most 64 bytes. Keys appear as OpenTelemetry attribute names
// (gateway.tag.<key>), so the alphabet is deliberately small.
var TagKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

var tagsDropped atomic.Uint64

// TagsDropped is the number of tag pairs discarded since process start
// because they were malformed, too long, duplicated or over the pair cap.
// Exported as gateway_llm_tags_dropped_total.
func TagsDropped() uint64 { return tagsDropped.Load() }

func countDropped(n int) {
	if n > 0 {
		tagsDropped.Add(uint64(n))
	}
}

// CapUser cleans a caller-supplied identity label for storage: surrounding
// space trimmed, invalid UTF-8 and control characters dropped, and the result
// cut to MaxUserLen bytes on a character boundary.
func CapUser(s string) string { return capText(s, MaxUserLen) }

func capText(s string, limit int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// validTagValue reports whether v can be stored as a tag value: non-empty,
// at most MaxTagValueLen bytes, valid UTF-8, no control characters.
func validTagValue(v string) bool {
	if v == "" || len(v) > MaxTagValueLen || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ParseTagsHeader parses an X-Gateway-Tags value, "k1=v1,k2=v2", into at most
// maxPairs tags (DefaultMaxTagPairs when maxPairs <= 0). Keys must match
// TagKeyPattern; values are percent-decoded (a "+" stays a plus). A pair
// that is malformed, has an invalid or empty value, repeats an earlier key
// or arrives after the cap is dropped, never an error: the others are kept.
// dropped counts the discarded pairs and is added to TagsDropped.
func ParseTagsHeader(header string, maxPairs int) (tags map[string]string, dropped int) {
	if maxPairs <= 0 {
		maxPairs = DefaultMaxTagPairs
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, 0
	}
	if len(header) > MaxTagsHeaderLen {
		cut := strings.LastIndexByte(header[:MaxTagsHeaderLen], ',')
		if cut < 0 {
			cut = 0
		}
		dropped += strings.Count(header[cut:], ",") // pairs after the cut
		if cut == 0 {
			dropped++ // no comma before the limit: the first pair itself is cut
		}
		if cut == 0 {
			countDropped(dropped)
			return nil, dropped
		}
		header = header[:cut]
	}
	for _, pair := range strings.Split(header, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || !TagKeyPattern.MatchString(k) {
			dropped++
			continue
		}
		dec, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil || !validTagValue(dec) {
			dropped++
			continue
		}
		if _, dup := tags[k]; dup || len(tags) >= maxPairs {
			dropped++
			continue
		}
		if tags == nil {
			tags = make(map[string]string)
		}
		tags[k] = dec
	}
	countDropped(dropped)
	return tags, dropped
}

// CleanTags applies the same limits to an already-decoded map (the "tags"
// field of an ingested llm_calls entry). Entries are considered in key order
// so the cut at maxPairs is deterministic. dropped is added to TagsDropped.
func CleanTags(in map[string]string, maxPairs int) (tags map[string]string, dropped int) {
	if maxPairs <= 0 {
		maxPairs = DefaultMaxTagPairs
	}
	if len(in) == 0 {
		return nil, 0
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := in[k]
		if !TagKeyPattern.MatchString(k) || !validTagValue(v) || len(tags) >= maxPairs {
			dropped++
			continue
		}
		if tags == nil {
			tags = make(map[string]string)
		}
		tags[k] = v
	}
	countDropped(dropped)
	return tags, dropped
}
