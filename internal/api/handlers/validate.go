package handlers

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
)

const (
	minNameLen = 1
	maxNameLen = 120

	minTimeoutMS = 100
	maxTimeoutMS = 600000
)

// errRequired returns a validation error for a missing/empty field.
func errRequired(field string) error {
	return fmt.Errorf("%s is required", field)
}

// validateName enforces the shared 1-120 character rule used for every
// named resource (tenant, API key, credential, connector, profile).
func validateName(field, name string) error {
	n := len([]rune(name))
	if n < minNameLen || n > maxNameLen {
		return fmt.Errorf("%s must be between %d and %d characters", field, minNameLen, maxNameLen)
	}
	return nil
}

// validateAbsoluteHTTPURL requires an absolute http(s) URL (scheme and
// host both present). ctx bounds the DNS lookup performed for a hostname
// (see netguard.CheckHostResolve); callers pass the request's context.
func validateAbsoluteHTTPURL(ctx context.Context, field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", field)
	}
	if u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", field)
	}
	// Fast feedback for an internal literal IP or a hostname that
	// resolves to one (e.g. "localhost"); the dial-time guard
	// (internal/netguard's Control hook) remains the authority either
	// way, so a name that can't be resolved here isn't rejected.
	if err := netguard.CheckHostResolve(ctx, u.Hostname()); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// validateTimeoutMS enforces the 100..600000ms range. A zero value means
// "not provided" and is left to the caller to default.
func validateTimeoutMS(ms int) error {
	if ms < minTimeoutMS || ms > maxTimeoutMS {
		return fmt.Errorf("timeout_ms must be between %d and %d", minTimeoutMS, maxTimeoutMS)
	}
	return nil
}

// validateRole enforces that role is one of allowed's keys -- the roles
// currently configured on the process's Authorizer (see Deps.AllowedRoles
// and router.go's allowedRoles), not a hardcoded {admin, agent} pair, so
// a custom role added via config.Auth.Roles is accepted here too. The
// error lists the known roles, sorted for a stable message.
func validateRole(role string, allowed map[string]bool) error {
	if allowed[role] {
		return nil
	}
	known := make([]string, 0, len(allowed))
	for r := range allowed {
		known = append(known, r)
	}
	sort.Strings(known)
	return fmt.Errorf("role must be one of: %s", strings.Join(known, ", "))
}

// slugPattern is the shape an explicit, caller-supplied slug override
// must match: lowercase letters, digits, and hyphens only (the same
// alphabet slugify ever produces, but not enforced to be its exact
// output -- a caller may supply any string that already fits it).
var slugPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// validateSlug enforces slugPattern (and, when maxLen > 0, a maximum
// length) for a non-empty, caller-supplied slug override. Callers decide
// separately whether an empty slug means "not provided" (the common
// case: fall back to a name-derived default).
func validateSlug(field, slug string, maxLen int) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("%s must contain only lowercase letters, numbers, and hyphens", field)
	}
	if maxLen > 0 && len(slug) > maxLen {
		return fmt.Errorf("%s must be at most %d characters", field, maxLen)
	}
	return nil
}

// slugify lowercases name and replaces every run of non-alphanumeric
// characters with a single "-", trims leading/trailing "-", and truncates
// to maxLen (0 means unbounded). An input that slugifies to nothing (e.g.
// all punctuation) falls back to "item" so callers never persist an empty
// slug.
func slugify(name string, maxLen int) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if maxLen > 0 && len(s) > maxLen {
		s = strings.Trim(s[:maxLen], "-")
	}
	if s == "" {
		s = "item"
	}
	return s
}
