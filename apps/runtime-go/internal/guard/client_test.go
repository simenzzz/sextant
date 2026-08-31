package guard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// The sidecar client, driven with httptest. It is on the hot path of every
// question and it is the guard's only source of evidence, so every failure
// path is asserted to fail closed: there must be no return value from Parse
// that a caller could mistake for approval.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// conformingSummary is what a healthy sidecar returns.
func conformingSummary() map[string]any {
	return map[string]any{
		"schema":          "parse_summary.v1",
		"ok":              true,
		"dialect":         "sqlite",
		"statement_count": 1,
		"node_kinds":      []string{"Select", "From", "Table", "Limit"},
		"tables":          []string{"orders"},
		"functions":       []string{},
		"has_limit":       false,
		"normalized_sql":  "SELECT id FROM orders LIMIT 500",
		"limit_injected":  true,
	}
}

func TestNewHTTPParserRefusesAnUnusableConfiguration(t *testing.T) {
	// An unbounded client on the hot path of every question would hang a run
	// past every cap meant to bound it, so the timeout is required rather than
	// defaulted.
	for _, tt := range []struct {
		name    string
		baseURL string
		timeout time.Duration
	}{
		{name: "no base URL", baseURL: "", timeout: time.Second},
		{name: "a blank base URL", baseURL: "   ", timeout: time.Second},
		{name: "no timeout", baseURL: "http://parser", timeout: 0},
		{name: "a negative timeout", baseURL: "http://parser", timeout: -time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewHTTPParser(tt.baseURL, tt.timeout, quietLogger()); err == nil {
				t.Fatal("NewHTTPParser() accepted an unusable configuration")
			}
		})
	}
}

func TestHTTPParserSendsTheStatementAndReturnsTheSummary(t *testing.T) {
	var got struct {
		Sql      string `json:"sql"`
		Dialect  string `json:"dialect"`
		LimitCap int    `json:"limit_cap"`
	}
	var path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(conformingSummary())
	}))
	defer srv.Close()

	// A trailing slash on the configured URL must not produce a double slash
	// in the path, which some routers treat as a different route.
	p, err := NewHTTPParser(srv.URL+"/", time.Second, quietLogger())
	if err != nil {
		t.Fatalf("NewHTTPParser() error = %v", err)
	}

	summary, err := p.Parse(context.Background(), "SELECT id FROM orders", gen.SqlPlanV1DialectSqlite, 500)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if path != "/v1/parse" {
		t.Errorf("called %q, want /v1/parse", path)
	}
	if got.Sql != "SELECT id FROM orders" || got.Dialect != "sqlite" || got.LimitCap != 500 {
		t.Errorf("sent %+v, want the statement, dialect, and cap it was given", got)
	}
	if !summary.Ok || summary.StatementCount != 1 {
		t.Errorf("summary = %+v, want the sidecar's own answer", summary)
	}
}

func TestHTTPParserFailsClosedOnEveryBadResponse(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "a non-200 status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":"request body is too large"}`, http.StatusRequestEntityTooLarge)
			},
		},
		{
			name: "a server error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
		},
		{
			name: "a body that is not JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "not json at all")
			},
		},
		{
			name: "a summary missing a required field",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				s := conformingSummary()
				delete(s, "statement_count")
				_ = json.NewEncoder(w).Encode(s)
			},
		},
		{
			name: "a summary with the wrong discriminator",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				s := conformingSummary()
				s["schema"] = "something_else.v1"
				_ = json.NewEncoder(w).Encode(s)
			},
		},
		{
			name: "a summary whose dialect is not in the enum",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				s := conformingSummary()
				s["dialect"] = "oracle"
				_ = json.NewEncoder(w).Encode(s)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			p, err := NewHTTPParser(srv.URL, time.Second, quietLogger())
			if err != nil {
				t.Fatalf("NewHTTPParser() error = %v", err)
			}

			summary, err := p.Parse(context.Background(), "SELECT 1", gen.SqlPlanV1DialectSqlite, 500)
			if err == nil {
				t.Fatal("Parse() returned no error, so a broken sidecar looked like an approval")
			}
			// The guard treats ok=false as a syntax error it can report. A
			// zero summary reaching it would be read as the parser refusing
			// the statement rather than as the parser being broken.
			if summary.Ok {
				t.Errorf("Parse() returned an ok summary alongside an error: %+v", summary)
			}
		})
	}
}

func TestHTTPParserNeverForwardsTheSidecarsOwnDiagnostic(t *testing.T) {
	// The body is the sidecar's diagnostic and the transport error names its
	// address. Both are logged; neither may reach a caller that renders to a
	// browser.
	const marker = "zzz-internal-detail-zzz"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, marker, http.StatusBadRequest)
	}))
	defer srv.Close()

	p, err := NewHTTPParser(srv.URL, time.Second, quietLogger())
	if err != nil {
		t.Fatalf("NewHTTPParser() error = %v", err)
	}

	_, err = p.Parse(context.Background(), "SELECT 1", gen.SqlPlanV1DialectSqlite, 500)
	if err == nil {
		t.Fatal("Parse() accepted a 400")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error forwarded the sidecar's body: %v", err)
	}
}

func TestHTTPParserFailsClosedWhenTheSidecarIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // Nothing is listening now.

	p, err := NewHTTPParser(base, time.Second, quietLogger())
	if err != nil {
		t.Fatalf("NewHTTPParser() error = %v", err)
	}

	summary, err := p.Parse(context.Background(), "SELECT 1", gen.SqlPlanV1DialectSqlite, 500)
	if err == nil {
		t.Fatal("Parse() returned no error with nothing listening")
	}
	if summary.Ok {
		t.Error("Parse() returned an ok summary with nothing listening")
	}
	// The detail names the sidecar's address, which is logged rather than
	// returned.
	if strings.Contains(err.Error(), base) {
		t.Errorf("error leaked the sidecar's address: %v", err)
	}
}

func TestHTTPParserStopsWhenTheRunIsCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(conformingSummary())
	}))
	defer srv.Close()

	p, err := NewHTTPParser(srv.URL, time.Second, quietLogger())
	if err != nil {
		t.Fatalf("NewHTTPParser() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Parse(ctx, "SELECT 1", gen.SqlPlanV1DialectSqlite, 500); err == nil {
		t.Fatal("Parse() ran a request for a cancelled run")
	}
}

func TestHTTPParserBoundsWhatTheSidecarMayReturn(t *testing.T) {
	// A wedged sidecar must not be able to make the runtime read an unbounded
	// body into memory on the request path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"schema":"parse_summary.v1","padding":"`)
		chunk := strings.Repeat("a", 64*1024)
		for range (maxParseResponseBytes / len(chunk)) + 2 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}`)
	}))
	defer srv.Close()

	p, err := NewHTTPParser(srv.URL, 5*time.Second, quietLogger())
	if err != nil {
		t.Fatalf("NewHTTPParser() error = %v", err)
	}

	// Truncated at the cap, the body is no longer valid JSON, so it fails
	// contract validation and the call fails closed.
	summary, err := p.Parse(context.Background(), "SELECT 1", gen.SqlPlanV1DialectSqlite, 500)
	if err == nil {
		t.Fatal("Parse() accepted an oversized response")
	}
	if summary.Ok {
		t.Errorf("Parse() returned an ok summary from an oversized response: %+v", summary)
	}
}
