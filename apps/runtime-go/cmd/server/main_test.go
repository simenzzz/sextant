package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/config"
)

func TestBuildProvider(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		wantErr    bool
		errMatches string
	}{
		{name: "fake is available", provider: config.ProviderFake},
		{
			name:       "anthropic is not implemented until P1",
			provider:   config.ProviderAnthropic,
			wantErr:    true,
			errMatches: "not implemented until P1",
		},
		{
			name:       "unknown provider is rejected",
			provider:   "openai",
			wantErr:    true,
			errMatches: "unknown provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildProvider(config.Config{Provider: tt.provider})

			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildProvider(%q) = nil error, want a failure", tt.provider)
				}
				if !strings.Contains(err.Error(), tt.errMatches) {
					t.Errorf("error = %q, want it to contain %q", err, tt.errMatches)
				}
				if got != nil {
					t.Error("buildProvider returned a provider alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildProvider(%q) = %v, want nil", tt.provider, err)
			}
			if got == nil {
				t.Error("buildProvider returned a nil provider and no error")
			}
		})
	}
}

func TestHandleHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("health response is not valid JSON: %v", err)
	}
	if body["status"] != "ok" || body["service"] != "runtime-go" {
		t.Errorf("body = %v, want service=runtime-go status=ok", body)
	}
}
