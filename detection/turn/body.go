package turn

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
)

// bodyTexts is every string of a JSON body that could hold a value to
// remove (minSharedSecret bytes or more), each once: the history, the
// system prompt, tool inputs and results, member names, whatever field
// they sit in. The decoder is lenient (a repeated name, invalid UTF-8),
// and a syntax error only stops it there: a body the extractor cannot read
// still gives up the strings before the error. A tool input (an "input"
// object, an "arguments" string) is also listed whole, as JSON, so a rule
// that needs a key's context ("api_key": "…") sees it.
func bodyTexts(body []byte) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if len(s) >= minSharedSecret && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	walkJSON(body, true, add)
	return out
}

// walkJSON calls add with each string of b; with inputs, each tool input
// whole as well (only at the top level: a nested input is walked, not
// listed again, so the work stays linear in the body).
func walkJSON(b []byte, inputs bool, add func(string)) {
	dec := jsontext.NewDecoder(bytes.NewReader(b),
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	var name string
	for {
		kind, n := dec.StackIndex(dec.StackDepth())
		value := kind == '{' && n%2 == 1 // the next token is a member's value
		if inputs && value && (name == "input" || name == "arguments") {
			if k := dec.PeekKind(); k == '{' || k == '[' {
				v, err := dec.ReadValue()
				if err != nil {
					return
				}
				if c, ok := canonicalJSON(v); ok {
					add(c)
				}
				walkJSON(v, false, add)
				continue
			}
		}
		tok, err := dec.ReadToken()
		if err != nil {
			return
		}
		if tok.Kind() != '"' {
			continue
		}
		s := tok.String()
		add(s)
		switch {
		case kind == '{' && n%2 == 0:
			name = s
		case inputs && value && name == "arguments":
			// OpenAI sends a call's arguments as a string of JSON.
			if c, ok := canonicalJSON([]byte(s)); ok {
				add(c)
				walkJSON([]byte(s), false, add)
			}
		}
	}
}

// canonicalJSON is raw re-encoded the one way the encoder writes it: no
// space between tokens, each string with the fewest escapes ("\/" is "/",
// "A" is "A"). A value has one encoding in this form, so a secret in
// a tool input can be found, and replaced, as it is written anywhere else.
// ok is false when raw is not a single valid JSON value.
func canonicalJSON(raw []byte) (string, bool) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	v, err := dec.ReadValue()
	if err != nil {
		return "", false
	}
	v = v.Clone() // the decoder reuses its buffer
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return "", false // more than one value
	}
	if v.Format() != nil {
		return "", false
	}
	return string(v), true
}
