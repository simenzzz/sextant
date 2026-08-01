package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv blanks every variable Load reads, so a test starts from a known
// state regardless of the developer's shell — which matters here, because a
// stray SEXTANT_* export would otherwise make these tests pass or fail for
// reasons that have nothing to do with the code. Load treats "" as unset, so
// blanking is equivalent to unsetting. t.Setenv restores on cleanup.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SEXTANT_ADDR", "SEXTANT_ALLOWED_ORIGINS", "SEXTANT_TRUST_PROXY",
		"SEXTANT_RETRIEVER_URL", "SEXTANT_TRACE_PATH",
		"SEXTANT_PROVIDER", "SEXTANT_PROVIDER_API_KEY", "SEXTANT_PROVIDER_BASE_URL",
		"SEXTANT_PROVIDER_TIMEOUT", "SEXTANT_CHEAP_MODEL", "SEXTANT_STRONG_MODEL",
		"SEXTANT_MAX_REPAIR_DEPTH", "SEXTANT_BUDGET_USD", "SEXTANT_WALL_CLOCK",
		"SEXTANT_ROW_LIMIT", "SEXTANT_STATEMENT_TIMEOUT", "SEXTANT_SHUTDOWN_GRACE",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with a clean environment: %v", err)
	}

	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.Provider != ProviderFake {
		t.Errorf("Provider = %q, want %q — a fresh clone must never default to a paid provider",
			cfg.Provider, ProviderFake)
	}
	if cfg.BudgetUSD != DefaultBudgetUSD {
		t.Errorf("BudgetUSD = %g, want %g", cfg.BudgetUSD, DefaultBudgetUSD)
	}
	if cfg.WallClock != DefaultWallClock {
		t.Errorf("WallClock = %s, want %s", cfg.WallClock, DefaultWallClock)
	}
	if cfg.MaxRepairDepth != DefaultMaxRepairDepth {
		t.Errorf("MaxRepairDepth = %d, want %d", cfg.MaxRepairDepth, DefaultMaxRepairDepth)
	}
	if len(cfg.AllowedOrigins) != 0 {
		t.Errorf("AllowedOrigins = %v, want empty — an unset allowlist must not mean allow-all",
			cfg.AllowedOrigins)
	}
}

// The house rule this whole package exists for: set-but-invalid is an error,
// never a silent fallback to the default.
func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantSub string
	}{
		{"non-numeric repair depth", "SEXTANT_MAX_REPAIR_DEPTH", "three", "invalid integer"},
		{"typo'd budget", "SEXTANT_BUDGET_USD", "0.O5", "invalid number"},
		{"unitless duration", "SEXTANT_WALL_CLOCK", "30", "invalid duration"},
		{"non-boolean trust proxy", "SEXTANT_TRUST_PROXY", "yes please", "invalid boolean"},
		{"unknown provider", "SEXTANT_PROVIDER", "openai", "unknown provider"},
		{"repair depth above ceiling", "SEXTANT_MAX_REPAIR_DEPTH", "99", "must be in [0,10]"},
		{"budget above ceiling", "SEXTANT_BUDGET_USD", "5", "must be in (0,1]"},
		{"zero budget", "SEXTANT_BUDGET_USD", "0", "must be in (0,1]"},
		{"negative row limit", "SEXTANT_ROW_LIMIT", "-1", "must be in [1,10000]"},
		{"zero statement timeout", "SEXTANT_STATEMENT_TIMEOUT", "0s", "must be in (0,2m0s]"},
		{"negative provider timeout", "SEXTANT_PROVIDER_TIMEOUT", "-5s", "must be positive"},
		{"wall clock above ceiling", "SEXTANT_WALL_CLOCK", "10m", "must be in (0,5m0s]"},
		{"row limit above the sql_plan.v1 ceiling", "SEXTANT_ROW_LIMIT", "99999", "must be in [1,10000]"},
		{"statement timeout above the sql_plan.v1 ceiling", "SEXTANT_STATEMENT_TIMEOUT", "5m", "must be in (0,2m0s]"},
		{"plaintext provider URL to a remote host", "SEXTANT_PROVIDER_BASE_URL", "http://attacker.example", "refusing plaintext"},
		{"provider URL with no scheme", "SEXTANT_PROVIDER_BASE_URL", "api.anthropic.com", "absolute http(s) URL"},
		{"retriever URL with no host", "SEXTANT_RETRIEVER_URL", "not-a-url", "absolute http(s) URL"},
		{"origin with a trailing slash", "SEXTANT_ALLOWED_ORIGINS", "http://localhost:5173/", "not a usable origin"},
		{"wildcard origin", "SEXTANT_ALLOWED_ORIGINS", "*", "not a usable origin"},
		{"origin with no scheme", "SEXTANT_ALLOWED_ORIGINS", "localhost:5173", "not a usable origin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q returned nil error, want a failure", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error = %q, want it to name the variable %q", err, tt.key)
			}
		})
	}
}

func TestAnthropicProviderRequiresAPIKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("SEXTANT_PROVIDER", ProviderAnthropic)

	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error, want a failure when the provider is real and no key is set")
	}

	t.Setenv("SEXTANT_PROVIDER_API_KEY", "sk-test-not-a-real-key")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with a key: %v", err)
	}
	if cfg.Provider != ProviderAnthropic {
		t.Errorf("Provider = %q, want %q", cfg.Provider, ProviderAnthropic)
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("SEXTANT_ADDR", ":9999")
	t.Setenv("SEXTANT_ALLOWED_ORIGINS", "https://a.example , ,https://b.example")
	t.Setenv("SEXTANT_TRUST_PROXY", "true")
	t.Setenv("SEXTANT_BUDGET_USD", "0.25")
	t.Setenv("SEXTANT_WALL_CLOCK", "45s")
	t.Setenv("SEXTANT_ROW_LIMIT", "42")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", cfg.Addr)
	}
	want := []string{"https://a.example", "https://b.example"}
	if len(cfg.AllowedOrigins) != len(want) {
		t.Fatalf("AllowedOrigins = %v, want %v (blank entries dropped)", cfg.AllowedOrigins, want)
	}
	for i, w := range want {
		if cfg.AllowedOrigins[i] != w {
			t.Errorf("AllowedOrigins[%d] = %q, want %q", i, cfg.AllowedOrigins[i], w)
		}
	}
	if !cfg.TrustProxy {
		t.Error("TrustProxy = false, want true")
	}
	if cfg.BudgetUSD != 0.25 {
		t.Errorf("BudgetUSD = %g, want 0.25", cfg.BudgetUSD)
	}
	if cfg.WallClock != 45*time.Second {
		t.Errorf("WallClock = %s, want 45s", cfg.WallClock)
	}
	if cfg.RowLimit != 42 {
		t.Errorf("RowLimit = %d, want 42", cfg.RowLimit)
	}
}

func TestParseOrigins(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"unset", "", 0},
		{"whitespace only", "   ", 0},
		{"single", "https://a.example", 1},
		{"commas and blanks", "https://a.example,,  ,https://b.example ", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseOrigins(tt.raw); len(got) != tt.want {
				t.Errorf("parseOrigins(%q) = %v, want %d entries", tt.raw, got, tt.want)
			}
		})
	}
}

// The record/replay harness at P2 needs a local plaintext stub, so loopback is
// carved out explicitly rather than by leaving the field unchecked.
func TestPlaintextProviderURLAllowedOnLoopback(t *testing.T) {
	for _, host := range []string{"http://localhost:9999", "http://127.0.0.1:9999"} {
		t.Run(host, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("SEXTANT_PROVIDER_BASE_URL", host)
			if _, err := Load(); err != nil {
				t.Fatalf("Load() = %v, want loopback plaintext to be accepted", err)
			}
		})
	}
}

// A credential must never render, however it is printed or serialized. The
// whole point of the type is that no future log call has to remember.
func TestSecretNeverRenders(t *testing.T) {
	const real = "sk-ant-super-secret"
	s := Secret(real)

	if got := s.String(); strings.Contains(got, real) {
		t.Errorf("String() = %q, leaks the credential", got)
	}
	if got := fmt.Sprintf("%v|%s", s, s); strings.Contains(got, real) {
		t.Errorf("fmt verbs leak the credential: %q", got)
	}
	if got := s.LogValue().String(); strings.Contains(got, real) {
		t.Errorf("LogValue() = %q, leaks the credential", got)
	}
	j, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(j), real) {
		t.Errorf("MarshalJSON() = %s, leaks the credential", j)
	}
	if s.Reveal() != real {
		t.Error("Reveal() must return the real value — it is the one intended accessor")
	}
}

// Logging the whole Config is the single most likely way a key escapes, so it
// is covered directly.
func TestConfigLogValueRedactsTheKey(t *testing.T) {
	const real = "sk-ant-super-secret"
	cfg := Config{Provider: ProviderAnthropic, ProviderAPIKey: Secret(real)}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("configuration loaded", "config", cfg)

	if strings.Contains(buf.String(), real) {
		t.Errorf("logging a Config leaked the credential: %s", buf.String())
	}
	if !strings.Contains(buf.String(), redacted) {
		t.Errorf("expected the redaction marker in %s", buf.String())
	}
}

// These ceilings exist to agree with packages/contracts. If a schema bound
// moves and the constant does not, the runtime happily accepts a value that
// builds a document failing its own contract at the next boundary.
func TestCeilingsMatchTheContracts(t *testing.T) {
	tests := []struct {
		name     string
		schema   string
		pointer  []string
		constant float64
	}{
		{"repair depth", "question_request.v1", []string{"max_repair_depth", "maximum"}, MaxRepairDepthCeiling},
		{"budget", "question_request.v1", []string{"budget_usd", "maximum"}, MaxBudgetUSDCeiling},
		{"wall clock ms", "question_request.v1", []string{"wall_clock_ms", "maximum"}, float64(MaxWallClockCeiling / time.Millisecond)},
		{"row limit", "sql_plan.v1", []string{"limit_value", "maximum"}, MaxRowLimitCeiling},
		{"statement timeout ms", "sql_plan.v1", []string{"statement_timeout_ms", "maximum"}, float64(MaxStatementTimeoutCeiling / time.Millisecond)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(
				"../../../../packages/contracts/schemas", tt.schema+".schema.json"))
			if err != nil {
				t.Fatalf("reading schema: %v", err)
			}
			var doc struct {
				Properties map[string]map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parsing schema: %v", err)
			}

			field, key := tt.pointer[0], tt.pointer[1]
			prop, ok := doc.Properties[field]
			if !ok {
				t.Fatalf("%s has no property %q", tt.schema, field)
			}
			got, ok := prop[key].(float64)
			if !ok {
				t.Fatalf("%s.%s has no numeric %q", tt.schema, field, key)
			}
			if got != tt.constant {
				t.Errorf("%s.%s.%s = %g but the config ceiling is %g — they must agree",
					tt.schema, field, key, got, tt.constant)
			}
		})
	}
}
