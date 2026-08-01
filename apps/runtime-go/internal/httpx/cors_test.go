package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestCORSAllowsListedOrigin(t *testing.T) {
	h := CORS([]string{"http://localhost:5173", "https://sextant.example"}, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q, want the echoed origin", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, want it to contain Origin — otherwise a shared cache can "+
			"serve one origin's allowed response to another", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestCORSRejectsUnlistedOrigin(t *testing.T) {
	h := CORS([]string{"http://localhost:5173"}, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty for an unlisted origin", got)
	}
	// The request still succeeds; the browser is what refuses to hand the
	// response to the page. Blocking server-side would break non-browser
	// clients for no gain.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Error("Vary: Origin must be set on rejections too, or a cache poisons across origins")
	}
}

// An unset allowlist means no cross-origin client, never every client.
func TestCORSEmptyAllowlistAllowsNothing(t *testing.T) {
	h := CORS(nil, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty", got)
	}
}

func TestCORSNeverEmitsWildcardOrCredentials(t *testing.T) {
	h := CORS([]string{"*"}, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// "*" is not a valid Origin header value, so it can only ever arrive here
	// as a misconfigured allowlist entry. It must not match anything.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty — a literal \"*\" entry must not grant access", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials = %q, want it never to be set", got)
	}
}

func TestCORSAnswersPreflight(t *testing.T) {
	h := CORS([]string{"http://localhost:5173"}, okHandler())

	req := httptest.NewRequest(http.MethodOptions, "/question", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Allow-Methods = %q, want it to include POST", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("preflight body = %q, want empty — the wrapped handler must not run", rec.Body.String())
	}
}

// Mutating the caller's slice after construction must not widen the allowlist
// of a server that is already serving.
func TestCORSCopiesTheAllowlist(t *testing.T) {
	origins := []string{"http://localhost:5173"}
	h := CORS(origins, okHandler())
	origins[0] = "https://evil.example"

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty — the allowlist was captured by reference", got)
	}
}

func TestIsValidOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"http://localhost:5173", true},
		{"https://sextant.example", true},
		{"https://sextant.example:8443", true},
		{"", false},
		{"*", false},
		{"localhost:5173", false},              // no scheme
		{"ftp://sextant.example", false},       // wrong scheme
		{"http://localhost:5173/", false},      // trailing slash never matches an Origin header
		{"https://sextant.example/app", false}, // path
	}
	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			if got := IsValidOrigin(tt.origin); got != tt.want {
				t.Errorf("IsValidOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}
