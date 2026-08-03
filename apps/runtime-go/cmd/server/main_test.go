package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/config"
)

func TestBuildProvider(t *testing.T) {
	// Enough to construct the real adapter. No network call is made here:
	// NewAnthropicProvider only builds a client, and the SDK does not dial
	// until a request is streamed.
	usable := config.Config{
		ProviderAPIKey:  config.Secret("test-key-not-real"),
		ProviderBaseURL: config.DefaultProviderBaseURL,
		ProviderTimeout: config.DefaultProviderTimeout,
	}
	withProvider := func(name string) config.Config {
		cfg := usable
		cfg.Provider = name
		return cfg
	}

	tests := []struct {
		name       string
		cfg        config.Config
		wantErr    bool
		errMatches string
	}{
		{name: "fake is available", cfg: withProvider(config.ProviderFake)},
		{name: "anthropic is available", cfg: withProvider(config.ProviderAnthropic)},
		{
			// The defect PLAN.md section 12 singles out. The adapter refuses
			// rather than defaulting, so a config that lost its timeout fails
			// at startup instead of shipping an unbounded provider client.
			name: "anthropic without a timeout is refused",
			cfg: config.Config{
				Provider:       config.ProviderAnthropic,
				ProviderAPIKey: config.Secret("k"),
			},
			wantErr:    true,
			errMatches: "positive HTTP timeout",
		},
		{
			name: "anthropic without a key is refused",
			cfg: config.Config{
				Provider:        config.ProviderAnthropic,
				ProviderTimeout: config.DefaultProviderTimeout,
			},
			wantErr:    true,
			errMatches: "API key",
		},
		{
			name:       "unknown provider is rejected",
			cfg:        withProvider("openai"),
			wantErr:    true,
			errMatches: "unknown provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildProvider(tt.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildProvider(%q) = nil error, want a failure", tt.cfg.Provider)
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
				t.Fatalf("buildProvider(%q) = %v, want nil", tt.cfg.Provider, err)
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
