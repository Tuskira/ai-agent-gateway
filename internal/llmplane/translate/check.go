package translate

import (
	"fmt"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/llm"
)

// The drop list: the ONLY request fields a Provider that cannot carry them may
// leave out. Each is a hint about delivery, caching, accounting or context
// trimming that does not change what the model is asked; everything else a
// Provider cannot carry is refused with 400 "unsupported_by_route: <field>"
// (see Check). Keep docs/llm-plane.md ("Refused, not dropped") in step.
var (
	// droppedRequestFields are top-level fields (Request.Extra keys), plus
	// metadata and top_k, which have neutral slots but no Chat equivalent.
	droppedRequestFields = map[string]string{
		"metadata":           "opaque client metadata; never changes the answer",
		"top_k":              "sampling cut-off most Chat vendors do not take",
		"cache_control":      "prompt-caching hint; the vendor caches (or not) on its own",
		"context_management": "server-side trimming of old tool results/thinking; the vendor sees the untrimmed context",
	}
	// droppedBlockFields are content-block fields (Block.Extra keys).
	droppedBlockFields = map[string]bool{"cache_control": true}
	// droppedToolFields are tool-definition fields (Tool.Extra keys): hints
	// about caching, schema enforcement, lazy loading and input streaming.
	droppedToolFields = map[string]bool{
		"cache_control": true, "strict": true, "defer_loading": true, "eager_input_streaming": true,
	}
)

// Also dropped by design (documented, not configurable): the effort hint when
// the target model has no reasoning-effort control; a thinking budget (the
// vendor's model reasons, or not, on its own); earlier-turn thinking and
// redacted_thinking blocks the target cannot replay (they are signed by, or
// opaque to, another vendor); citations on earlier assistant text.

// Check walks req and returns an *llm.ErrUnsupported naming the first field
// caps cannot carry, or nil. A Passthrough provider carries everything.
func Check(req *llm.Request, caps llm.Capabilities) error {
	if caps.Passthrough {
		return nil
	}
	refuse := func(format string, a ...any) error { return &llm.ErrUnsupported{Field: fmt.Sprintf(format, a...)} }

	for _, k := range sortedKeys(req.Extra) {
		if _, ok := droppedRequestFields[k]; !ok {
			if k == "output_config" {
				return refuse("output_config.%s", strings.Join(sortedKeys(rawObject(req.Extra[k])), ","))
			}
			return refuse("%s", k)
		}
	}
	if th := req.Thinking; th != nil && th.Type != "disabled" && !caps.Thinking {
		return refuse("thinking")
	}
	if len(req.StopSequences) > 0 && !caps.StopSequences {
		return refuse("stop_sequences")
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto", "any", "tool":
		case "none":
			if !caps.ToolChoiceNone {
				return refuse("tool_choice.none")
			}
		default:
			return refuse("tool_choice.type=%s", tc.Type)
		}
		if tc.DisableParallelToolUse && !caps.ParallelToolControl {
			return refuse("tool_choice.disable_parallel_tool_use")
		}
	}
	for i, t := range req.Tools {
		if t.Type != "" && t.Type != "custom" && !caps.ServerTools {
			return refuse("tools[%d] (%s)", i, t.Type)
		}
		for _, k := range sortedKeys(t.Extra) {
			if !droppedToolFields[k] {
				return refuse("tools[%d].%s", i, k)
			}
		}
	}
	for i, b := range req.System {
		if b.Type != llm.BlockText {
			return refuse("system[%d] (%s)", i, b.Type)
		}
		if err := checkExtra(b, fmt.Sprintf("system[%d]", i), false); err != nil {
			return err
		}
	}
	for i, m := range req.Messages {
		for j, b := range m.Content {
			if err := checkBlock(b, m.Role, fmt.Sprintf("messages[%d].content[%d]", i, j), caps, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkBlock checks one content block of a turn with role; inResult is set
// inside a tool_result's content.
func checkBlock(b llm.Block, role, path string, caps llm.Capabilities, inResult bool) error {
	refuse := func(what string) error { return &llm.ErrUnsupported{Field: path + " (" + what + ")"} }
	if len(b.Raw) > 0 {
		return refuse(string(b.Type))
	}
	assistant := role == "assistant"
	switch b.Type {
	case llm.BlockText:
	case llm.BlockImage:
		if err := checkImage(b, caps); err != "" {
			return refuse(err)
		}
	case llm.BlockDocument:
		if assistant || inResult {
			return refuse("document")
		}
		switch {
		case b.Source == nil:
			return refuse("document without source")
		case b.Source.Type == "text":
			if !caps.TextDocuments {
				return refuse("text document")
			}
		case !caps.Documents:
			return refuse("document source " + b.Source.Type)
		}
	case llm.BlockToolUse:
		if !assistant || inResult {
			return refuse("tool_use outside an assistant turn")
		}
	case llm.BlockToolResult:
		if assistant || inResult {
			return refuse("tool_result outside a user turn")
		}
		for k, c := range b.Content {
			if c.Type != llm.BlockText && c.Type != llm.BlockImage && c.Type != llm.BlockToolReference {
				return (&llm.ErrUnsupported{Field: fmt.Sprintf("%s.content[%d] (%s)", path, k, c.Type)})
			}
			if err := checkBlock(c, role, fmt.Sprintf("%s.content[%d]", path, k), caps, true); err != nil {
				return err
			}
		}
	case llm.BlockToolReference:
		if !inResult || !caps.ToolReferences {
			return refuse("tool_reference")
		}
	case llm.BlockThinking, llm.BlockRedactedThinking:
		// Earlier-turn reasoning: the Provider replays what it can and drops
		// the rest (drop list). Outside an assistant turn it is malformed.
		if !assistant {
			return refuse(string(b.Type) + " outside an assistant turn")
		}
	default:
		return refuse(string(b.Type))
	}
	return checkExtra(b, path, assistant)
}

func checkImage(b llm.Block, caps llm.Capabilities) string {
	switch {
	case b.Source == nil:
		return "image without source"
	case b.Source.Type == "base64":
		if !strings.HasPrefix(b.Source.MediaType, "image/") && b.Source.MediaType != "" {
			return "image media_type " + b.Source.MediaType
		}
		if !caps.ImagesBase64 {
			return "image source base64"
		}
	case b.Source.Type == "url":
		if !caps.ImagesURL {
			return "image source url"
		}
	default:
		return "image source " + b.Source.Type
	}
	return ""
}

// checkExtra refuses block fields outside the drop list. Citations on an
// earlier assistant turn are annotations of text the vendor still gets.
func checkExtra(b llm.Block, path string, assistant bool) error {
	for _, k := range sortedKeys(b.Extra) {
		if droppedBlockFields[k] || (assistant && k == "citations") {
			continue
		}
		return &llm.ErrUnsupported{Field: path + "." + k}
	}
	return nil
}
