package handlers

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// containmentRequest is the POST /api-keys/{id}/containment body.
type containmentRequest struct {
	// Level is the rung: "normal", "restrict" (tool calls refused) or
	// "kill" (every call refused).
	Level string `json:"level"`
	// Note says why; required to restrict or kill. Reason is accepted as
	// an alias.
	Note   string `json:"note"`
	Reason string `json:"reason"`
}

// containmentView is the key after a containment change, with how many of
// its open calls were cut per plane.
type containmentView struct {
	apiKeyView
	Cut map[string]int `json:"cut"`
}

var containmentLevels = map[string]store.Containment{
	"normal":   store.ContainmentNormal,
	"restrict": store.ContainmentRestricted,
	"kill":     store.ContainmentKilled,
}

// Containment handles POST /api/v1/api-keys/{id}/containment: move an
// agent's key along the containment ladder -- normal, restrict (it may
// still call models; the MCP plane refuses every tool call) or kill (it is
// refused on every plane, like a revoked key, until restored) -- with a
// note. Takes effect on the key's next request; the key's open calls on
// this process's planes are cut at once (kill: LLM and MCP; restrict: MCP).
func (h APIKeys) Containment(w http.ResponseWriter, r *http.Request) {
	var req containmentRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	note := strings.TrimSpace(req.Note)
	if note == "" {
		note = strings.TrimSpace(req.Reason)
	}
	h.contain(w, r, req.Level, note)
}

// Suspend handles POST /api/v1/api-keys/{id}/suspend: containment level
// kill, with the note "suspended".
func (h APIKeys) Suspend(w http.ResponseWriter, r *http.Request) {
	h.contain(w, r, "kill", "suspended")
}

// Resume handles POST /api/v1/api-keys/{id}/resume: containment level
// normal.
func (h APIKeys) Resume(w http.ResponseWriter, r *http.Request) { h.contain(w, r, "normal", "resumed") }

func (h APIKeys) contain(w http.ResponseWriter, r *http.Request, levelName, note string) {
	tid, ok := requireTenant(w, r)
	if !ok {
		return
	}
	level, ok := containmentLevels[levelName]
	if !ok {
		httpx.ValidationError(w, "level must be normal, restrict or kill")
		return
	}
	if level != store.ContainmentNormal && note == "" {
		httpx.ValidationError(w, "say why: a note is required to restrict or kill an agent")
		return
	}
	ks, ok := h.Store.APIKeys().(store.APIKeyContainer)
	if !ok {
		httpx.WriteError(w, http.StatusNotImplemented, "not_implemented", "this store cannot contain keys")
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.Store.APIKeys().GetByID(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "contain api key", err)
		return
	}
	if p, ok := pkgauth.PrincipalFrom(r.Context()); level != store.ContainmentNormal && ok && p != nil && p.KeyID == existing.ID {
		httpx.ValidationError(w, "you can't contain the key you are signed in with")
		return
	}
	if err := ks.SetContainment(r.Context(), tid, id, level, note); err != nil {
		writeStoreErr(w, r, "contain api key", err)
		return
	}
	if h.KeyInvalidator != nil {
		h.KeyInvalidator.InvalidateKey(existing.KeyHash)
	}
	// Cut what the agent has open right now: everything on kill, tool
	// calls on restrict. Its next request is refused by the authenticator
	// or the MCP plane.
	cut := map[string]int{"llm": 0, "mcp": 0}
	switch level {
	case store.ContainmentKilled:
		cut["llm"], cut["mcp"] = h.Inflight.CancelKey(id, "llm"), h.Inflight.CancelKey(id, "mcp")
	case store.ContainmentRestricted:
		cut["mcp"] = h.Inflight.CancelKey(id, "mcp")
	}
	k, err := h.Store.APIKeys().GetByID(r.Context(), tid, id)
	if err != nil {
		writeStoreErr(w, r, "contain api key", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, containmentView{apiKeyView: h.withProfileName(r, tid, newAPIKeyView(k)), Cut: cut})
}
