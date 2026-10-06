// Package strictjson rejects JSON that two parsers could read differently,
// for the pkg/llm Readers.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// maxDepth bounds nesting, as encoding/json does when it decodes.
const maxDepth = 10000

// KeyError reports an object holding two keys a parser could confuse: the
// same key twice (most parsers keep the last, some the first), or two keys
// that differ only by case (Go's struct decoding matches either).
type KeyError struct {
	// Path locates the object, e.g. "messages.0.content.1" ("" for the top).
	Path string
	// First and Second are the two keys, in order.
	First, Second string
}

func (e *KeyError) Error() string {
	where := "top-level object"
	if e.Path != "" {
		where = e.Path
	}
	if e.First == e.Second {
		return fmt.Sprintf("duplicate key %q in %s", e.First, where)
	}
	return fmt.Sprintf("keys %q and %q in %s differ only by case", e.First, e.Second, where)
}

// Check returns a *KeyError for the first ambiguous object in data, and nil
// otherwise. It does not judge syntax: a body that is not valid JSON is
// left to the parser that reads it next (and is checked up to the error).
func Check(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	err := value(dec, "", 0)
	if _, ok := err.(*KeyError); ok {
		return err
	}
	return nil
}

func value(dec *json.Decoder, path string, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxDepth {
		return fmt.Errorf("exceeded max depth %d", maxDepth)
	}
	switch d {
	case '{':
		seen := map[string]string{}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := tok.(string)
			if !ok {
				return fmt.Errorf("object key is %T", tok)
			}
			fold := strings.ToLower(strings.ToUpper(key))
			if first, dup := seen[fold]; dup {
				return &KeyError{Path: path, First: first, Second: key}
			}
			seen[fold] = key
			if err := value(dec, join(path, key), depth+1); err != nil {
				return err
			}
		}
	case '[':
		for i := 0; dec.More(); i++ {
			if err := value(dec, join(path, strconv.Itoa(i)), depth+1); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // the closing delimiter
	return err
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
