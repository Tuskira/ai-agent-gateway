// Package gemini is the llm.Reader for the Gemini API wire format
// (generateContent and streamGenerateContent). It registers itself as
// Reader "gemini" from an init(); a binary blank-imports it to use it.
package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm/internal/strictjson"
)

// Name is the registry name of the Reader.
const Name = "gemini"

func init() { llm.RegisterReader(Reader{}) }

// Reader is the llm.Reader for the Gemini API: a client's generateContent
// request body, and the response or stream (SSE with alt=sse, or the JSON
// array streamGenerateContent sends without it) it got back.
//
// The model is named in the URL path, not the body, so DecodeRequest leaves
// Request.Model empty (it reads a "model" field only if the body has one) and
// Request.Stream false: the caller, who sees the path, sets both.
//
// Field names are read in either spelling the API accepts, lowerCamel
// (maxOutputTokens) or snake_case (max_output_tokens); an object that spells
// one field both ways is refused, like a repeated key. Extra keys are the
// lowerCamel names. Values the API treats as opaque (function arguments and
// responses, schemas) are kept verbatim.
//
// Mapping. systemInstruction's text parts are the system prompt. Contents
// roles are "user" (or unset) and "model" (the assistant); the legacy
// "function" role, holding only functionResponse parts, is a user turn; any
// other role is refused. Parts: text (a thinking block when "thought" is
// set, its thoughtSignature the block's signature), inlineData (an image
// block for an image/* MIME type, else a document block, with a base64
// source), fileData (likewise, with a "file" source whose FileID is the
// fileUri), functionCall (tool_use) and functionResponse (tool_result whose
// content is the response object as JSON text, then any media parts;
// IsError when the response object has an "error" key). executableCode and
// codeExecutionResult are kept whole in Raw. Fields of a part with no
// neutral slot (thoughtSignature on a non-thought part, videoMetadata, ...)
// ride in its block's Extra; fields of a content other than role and parts
// in the Extra of its first block.
//
// Tool call ids. Gemini usually sends no ids, so a functionCall without one
// gets "gemini-call-<n>-<name>", n counting the functionCall parts of the
// body from 0. A functionResponse without an id answers the first unanswered
// call of the same name in the latest model turn (the API requires the
// responses to follow their calls), and one that answers no call is refused.
// An id the body does carry is used as is.
//
// tools[].functionDeclarations are Tools; any other key of a tools entry
// (googleSearch, codeExecution, ...) is a vendor-defined Tool whose Type and
// Name are the key and whose Extra holds it. toolConfig.functionCallingConfig
// mode AUTO, ANY and NONE are ToolChoice auto, any and none (ANY with one
// allowed function name is tool choice of that name), VALIDATED is auto with
// the mode kept; what is not mapped stays in Extra["toolConfig"].
// generationConfig maxOutputTokens, temperature, topP, topK and
// stopSequences fill their neutral fields, thinkingConfig.thinkingBudget is
// Thinking (-1 adaptive, 0 disabled, else enabled with that budget) and
// thinkingConfig.thinkingLevel low, medium or high is Effort; the rest stays
// in Extra["generationConfig"]. Other top-level fields go to Extra.
type Reader struct{}

func (Reader) Name() string { return Name }

// DecodeRequest reads a generateContent request body strictly.
func (Reader) DecodeRequest(body []byte) (*llm.Request, error) {
	if err := strictjson.Check(body); err != nil {
		return nil, &llm.RequestError{Err: fmt.Errorf("invalid request body: %v", err)}
	}
	req, err := readRequest(body)
	if err != nil {
		return nil, &llm.RequestError{Err: err}
	}
	return req, nil
}

// DecodeResponse reads a non-stream generateContent response body: its first
// candidate, usageMetadata as usage, responseId and modelVersion as the id
// and model.
func (Reader) DecodeResponse(body []byte) (*llm.Response, error) {
	if len(body) > llm.MaxBodyBytes {
		return nil, llm.ErrFrameTooLarge
	}
	if err := strictjson.Check(body); err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	c, err := readChunk(body, &calls{})
	if err != nil {
		return nil, fmt.Errorf("invalid response body: %v", err)
	}
	if c.err != nil {
		return nil, c.err
	}
	out := &llm.Response{ID: c.id, Model: c.model, Role: "assistant", Content: c.parts, StopReason: c.stopReason(c.sawTool())}
	if out.Content == nil {
		out.Content = []llm.Block{}
	}
	if c.usage != nil {
		out.Usage = *c.usage
	}
	return out, nil
}

// NewResponseDecoder reads a streamGenerateContent response: SSE data
// frames, or, when the body starts with "[", a JSON array of the same
// chunks. Each frame is checked strictly.
func (Reader) NewResponseDecoder(r io.Reader) llm.StreamDecoder { return newDecoder(r) }

// object decodes a JSON object with its keys in lowerCamel form; anything
// else, or two keys naming the same field, is an error.
func object(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(m))
	wire := make(map[string]string, len(m))
	for k, v := range m {
		c := camel(k)
		if first, dup := wire[c]; dup {
			pair := []string{first, k}
			sort.Strings(pair)
			return nil, fmt.Errorf("keys %q and %q name the same field", pair[0], pair[1])
		}
		out[c], wire[c] = v, k
	}
	return out, nil
}

// camel returns the lowerCamel form of a snake_case field name, as the
// API's JSON parser maps them.
func camel(k string) string {
	if !strings.Contains(k, "_") {
		return k
	}
	var b strings.Builder
	up := false
	for _, r := range k {
		switch {
		case r == '_':
			up = true
		case up:
			b.WriteString(strings.ToUpper(string(r)))
			up = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// take moves m[key] into dst (a null or missing value leaves dst alone).
func take(m map[string]json.RawMessage, key string, dst any) error {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	delete(m, key)
	if isNull(raw) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: %v", key, err)
	}
	return nil
}

// takeObject moves m[key] out as an object (nil when null or missing).
func takeObject(m map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	raw, ok := m[key]
	if !ok {
		return nil, nil
	}
	delete(m, key)
	if isNull(raw) {
		return nil, nil
	}
	o, err := object(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", key, err)
	}
	return o, nil
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func nonEmpty(m map[string]json.RawMessage) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return m
}

// merge copies src into *dst, allocating it when needed.
func merge(dst *map[string]json.RawMessage, src map[string]json.RawMessage) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = map[string]json.RawMessage{}
	}
	for k, v := range src {
		(*dst)[k] = v
	}
}

func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}
