package httpx

import (
	"net/http"
	"slices"
	"strings"
)

// CORS wraps h with an exact-match origin allowlist.
//
// Three deliberate properties:
//
//   - It never emits `Access-Control-Allow-Origin: *`. The matched origin is
//     echoed back instead, so the allowlist is the only thing that widens
//     access and widening it is a config change, not a code change.
//   - An empty allowlist allows nothing. An unset variable means "no
//     cross-origin browser client", never "allow everything".
//   - Credentials are never allowed. The API is bearer-free and same-site
//     cookies play no part; permitting credentials would turn any allowlisted
//     origin into a confused deputy.
//
// `Vary: Origin` is always set, including on rejections, so a shared cache can
// never serve one origin's allowed response to another origin.
func CORS(allowedOrigins []string, h http.Handler) http.Handler {
	// Copied so a later mutation of the caller's slice cannot silently widen
	// the allowlist of an already-running server.
	allowed := slices.Clone(allowedOrigins)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")

		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}

		// A preflight is answered whether or not the origin matched. Without
		// the allow headers above the browser rejects it, which is the correct
		// outcome and a clearer one than a 404 on OPTIONS.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// IsValidOrigin reports whether s is a usable CORS origin: scheme://host with
// no path, no trailing slash, and no wildcard.
//
// Browsers send exactly this form in the Origin header, so an entry in any
// other shape can never match and would sit in the config looking like it
// grants access while granting none.
func IsValidOrigin(s string) bool {
	if s == "" || s == "*" {
		return false
	}
	scheme, rest, found := strings.Cut(s, "://")
	if !found || (scheme != "http" && scheme != "https") {
		return false
	}
	// Any remaining slash means a path was included; Origin headers carry none.
	return rest != "" && !strings.Contains(rest, "/")
}
