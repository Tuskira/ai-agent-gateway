package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/api/httpx"
	pkgauth "github.com/Tuskira/tusk-ai-secured-gateway/pkg/auth"
	pkgheaders "github.com/Tuskira/tusk-ai-secured-gateway/pkg/headers"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Platform-locked rows. A credential or credential source with locked=true
// is provisioned by a platform operator. A caller without platform.admin (a
// tenant admin) cannot change, rotate, disable, delete or re-point it, and
// cannot set or clear the flag. Connector headers may name a locked source.
// A locked credential's value is sent only where the platform chose: through
// a locked source, a header a platform admin saved (pkgheaders.AllowLockedKey)
// or a platform model; secrets.Service enforces that on every read.

const managedByPlatformMsg = "managed by the platform"

// isPlatformAdmin reports whether the caller holds platform.admin. It fails
// closed: no authorizer or no principal means no.
func (d Deps) isPlatformAdmin(r *http.Request) bool {
	if d.Authorizer == nil {
		return false
	}
	p, ok := pkgauth.PrincipalFrom(r.Context())
	if !ok || p == nil {
		return false
	}
	return d.Authorizer.Allow(r.Context(), p, pkgauth.PermPlatformAdmin)
}

// denyLockedChange writes 403 permission_error when the row is locked and
// the caller is not a platform admin; it reports whether it refused.
func (d Deps) denyLockedChange(w http.ResponseWriter, r *http.Request, locked bool) bool {
	if !locked || d.isPlatformAdmin(r) {
		return false
	}
	httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, managedByPlatformMsg)
	return true
}

// denySetLocked writes 403 permission_error when the caller asks to set or
// clear the locked flag (want differs from current) without platform.admin.
func (d Deps) denySetLocked(w http.ResponseWriter, r *http.Request, want, current bool) bool {
	if want == current || d.isPlatformAdmin(r) {
		return false
	}
	httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission, "setting or clearing locked requires platform.admin")
	return true
}

// guardLockedHeaders applies the platform-lock rules to a connector write.
// headers is the connector's metadata.headers as it will be saved; stored
// is the same map before the write (nil on create). A header is examined
// when it differs from stored, or always when the caller is a platform
// admin (who thereby approves every header of the connector they save).
// The server-owned pkgheaders.AllowLockedKey is removed from it (a client
// can never forge it); then, when the header names a platform-locked
// credential (pkgheaders.CredentialReferencer), a caller without
// platform.admin gets 403 and a platform admin's header is stamped with it,
// which is what lets it resolve that credential at request time. For any
// other caller a header equal to stored keeps whatever it had. It reports
// whether it refused.
func (d Deps) guardLockedHeaders(w http.ResponseWriter, r *http.Request, tid string, headers, stored map[string]any) bool {
	admin := d.isPlatformAdmin(r)
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg, ok := headers[name].(map[string]any)
		if !ok || (!admin && reflect.DeepEqual(cfg, stored[name])) {
			continue
		}
		delete(cfg, pkgheaders.AllowLockedKey)
		locked, err := d.namesLockedCredential(r.Context(), tid, cfg)
		if err != nil {
			writeStoreErr(w, r, "check header credentials", err)
			return true
		}
		if !locked {
			continue
		}
		if !admin {
			httpx.WriteError(w, http.StatusForbidden, httpx.TypePermission,
				fmt.Sprintf("header %q: its credential is %s and cannot be used in a connector header", name, managedByPlatformMsg))
			return true
		}
		cfg[pkgheaders.AllowLockedKey] = true
	}
	return false
}

// namesLockedCredential reports whether a header config names a
// platform-locked credential of tenant tid.
func (d Deps) namesLockedCredential(ctx context.Context, tid string, cfg map[string]any) (bool, error) {
	for _, cred := range d.Headers.ReferencedCredentials(cfg) {
		c, err := d.Store.Credentials().Get(ctx, tid, cred)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if c.Locked {
			return true, nil
		}
	}
	return false, nil
}

// hasPlatformHeader reports whether any header of headers carries the
// platform admin's pkgheaders.AllowLockedKey stamp.
func hasPlatformHeader(headers map[string]any) bool {
	for _, raw := range headers {
		if cfg, ok := raw.(map[string]any); ok {
			if v, _ := cfg[pkgheaders.AllowLockedKey].(bool); v {
				return true
			}
		}
	}
	return false
}

// metadataChangedBeyondToggle reports whether next differs from prev in any
// key other than enabled and description (headers, tls, ...).
func metadataChangedBeyondToggle(next, prev map[string]any) bool {
	strip := func(m map[string]any) map[string]any {
		out := make(map[string]any, len(m))
		for k, v := range m {
			if k == "enabled" || k == "description" {
				continue
			}
			out[k] = v
		}
		return out
	}
	return !reflect.DeepEqual(strip(next), strip(prev))
}
