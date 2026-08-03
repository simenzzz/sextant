// Package config loads and validates the runtime's configuration from the
// environment.
//
// The house rule, one line: an unset variable takes its default; a variable
// that is set but unparseable is an error. Silently falling back to a default
// when someone typed SEXTANT_BUDGET_USD=0.O5 is how a cost cap stops existing
// without anyone noticing.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/httpx"
)

// Defaults. Every one of these is overridable; none is written inline at a use
// site. The three caps come from PLAN.md 5.3 and bound every agent run.
const (
	DefaultAddr             = ":8080"
	DefaultRetrieverURL     = "http://localhost:8000"
	DefaultTracePath        = "sextant-trace.sqlite"
	DefaultProvider         = ProviderFake
	DefaultCheapModel       = "claude-haiku-4-5-20251001"
	DefaultStrongModel      = "claude-sonnet-5"
	DefaultProviderBaseURL  = "https://api.anthropic.com"
	DefaultProviderTimeout  = 60 * time.Second
	DefaultMaxRepairDepth   = 3
	DefaultBudgetUSD        = 0.05
	DefaultWallClock        = 30 * time.Second
	DefaultRowLimit         = 500
	DefaultStatementTimeout = 10 * time.Second
	DefaultShutdownGrace    = 10 * time.Second

	// P1 additions.
	DefaultDatabases         = "toy=sqlite:../../infra/fixtures/toy.sqlite"
	DefaultParserTimeout     = 5 * time.Second
	DefaultMaxConcurrentRuns = 8
	DefaultRateLimitBurst    = 5
	DefaultRateLimitPerMin   = 20.0
	DefaultMaxResultBytes    = 1 << 20
)

// Provider names the LLM backend. Defaulting to the fake keeps `go test`,
// `docker compose up`, and a fresh clone working with no API key and no
// possibility of an accidental paid call.
const (
	ProviderFake      = "fake"
	ProviderAnthropic = "anthropic"
)

// Ceilings. These are not free-floating opinions: each mirrors a bound in
// packages/contracts, and config_test.go asserts they still agree. A runtime
// that accepts a row limit above sql_plan.v1's maximum would happily build a
// plan that fails its own contract at the guard.
const (
	// Mirror question_request.v1.
	MaxRepairDepthCeiling = 10
	MaxBudgetUSDCeiling   = 1.0
	MaxWallClockCeiling   = 5 * time.Minute
	// Mirror sql_plan.v1.
	MaxRowLimitCeiling         = 10000
	MaxStatementTimeoutCeiling = 2 * time.Minute
	// Must equal MAX_RESULT_FRAME_CHARS in apps/web/src/lib/protocol.ts and
	// executor.MaxResultBytesCeiling. A runtime permitted to emit a larger
	// frame would produce one its own browser client rejects.
	MaxResultBytesCeiling = 4 << 20
)

// Config is the fully validated runtime configuration. Every field is
// exported and plain: nothing here reads the environment after Load returns.
type Config struct {
	Addr           string
	AllowedOrigins []string
	TrustProxy     bool

	RetrieverURL string
	TracePath    string

	Provider        string
	ProviderAPIKey  Secret
	ProviderBaseURL string
	ProviderTimeout time.Duration
	CheapModel      string
	StrongModel     string

	MaxRepairDepth   int
	BudgetUSD        float64
	WallClock        time.Duration
	RowLimit         int
	StatementTimeout time.Duration

	// Databases is the slug=dialect:dsn registry the question endpoint
	// resolves against. Empty configures nothing, and the runtime then serves
	// no questions rather than guessing at a database.
	Databases string
	// ParserTimeout bounds the call to the retriever sidecar's /v1/parse,
	// which sits on the hot path of every question.
	ParserTimeout time.Duration
	// MaxConcurrentRuns bounds in-flight agent loops, and so bounds concurrent
	// provider connections.
	MaxConcurrentRuns int
	// RateLimitBurst and RateLimitPerMinute bound one client.
	RateLimitBurst     int
	RateLimitPerMinute float64
	// MaxResultBytes bounds the streamed result frame.
	MaxResultBytes int

	ShutdownGrace time.Duration
}

// Load reads configuration from the environment and validates it.
//
// Range checks live here rather than in the typed helpers below, so the
// helpers stay about parsing and this function stays the single place that
// decides what a legal configuration is.
func Load() (Config, error) {
	cfg := Config{
		Addr:            orDefault(os.Getenv("SEXTANT_ADDR"), DefaultAddr),
		AllowedOrigins:  parseOrigins(os.Getenv("SEXTANT_ALLOWED_ORIGINS")),
		RetrieverURL:    orDefault(os.Getenv("SEXTANT_RETRIEVER_URL"), DefaultRetrieverURL),
		TracePath:       orDefault(os.Getenv("SEXTANT_TRACE_PATH"), DefaultTracePath),
		Provider:        orDefault(os.Getenv("SEXTANT_PROVIDER"), DefaultProvider),
		ProviderAPIKey:  Secret(os.Getenv("SEXTANT_PROVIDER_API_KEY")),
		ProviderBaseURL: orDefault(os.Getenv("SEXTANT_PROVIDER_BASE_URL"), DefaultProviderBaseURL),
		CheapModel:      orDefault(os.Getenv("SEXTANT_CHEAP_MODEL"), DefaultCheapModel),
		StrongModel:     orDefault(os.Getenv("SEXTANT_STRONG_MODEL"), DefaultStrongModel),
		Databases:       orDefault(os.Getenv("SEXTANT_DATABASES"), DefaultDatabases),
	}

	var err error
	if cfg.TrustProxy, err = envBool("SEXTANT_TRUST_PROXY", false); err != nil {
		return Config{}, err
	}
	if cfg.ProviderTimeout, err = envDuration("SEXTANT_PROVIDER_TIMEOUT", DefaultProviderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxRepairDepth, err = envInt("SEXTANT_MAX_REPAIR_DEPTH", DefaultMaxRepairDepth); err != nil {
		return Config{}, err
	}
	if cfg.BudgetUSD, err = envFloat("SEXTANT_BUDGET_USD", DefaultBudgetUSD); err != nil {
		return Config{}, err
	}
	if cfg.WallClock, err = envDuration("SEXTANT_WALL_CLOCK", DefaultWallClock); err != nil {
		return Config{}, err
	}
	if cfg.RowLimit, err = envInt("SEXTANT_ROW_LIMIT", DefaultRowLimit); err != nil {
		return Config{}, err
	}
	if cfg.StatementTimeout, err = envDuration("SEXTANT_STATEMENT_TIMEOUT", DefaultStatementTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownGrace, err = envDuration("SEXTANT_SHUTDOWN_GRACE", DefaultShutdownGrace); err != nil {
		return Config{}, err
	}
	if cfg.ParserTimeout, err = envDuration("SEXTANT_PARSER_TIMEOUT", DefaultParserTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxConcurrentRuns, err = envInt("SEXTANT_MAX_CONCURRENT_RUNS", DefaultMaxConcurrentRuns); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitBurst, err = envInt("SEXTANT_RATE_LIMIT_BURST", DefaultRateLimitBurst); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitPerMinute, err = envFloat("SEXTANT_RATE_LIMIT_PER_MINUTE", DefaultRateLimitPerMin); err != nil {
		return Config{}, err
	}
	if cfg.MaxResultBytes, err = envInt("SEXTANT_MAX_RESULT_BYTES", DefaultMaxResultBytes); err != nil {
		return Config{}, err
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch c.Provider {
	case ProviderFake:
		// No credential needed, and none should be supplied.
	case ProviderAnthropic:
		if c.ProviderAPIKey.IsZero() {
			return fmt.Errorf("SEXTANT_PROVIDER_API_KEY is required when SEXTANT_PROVIDER=%s", c.Provider)
		}
	default:
		return fmt.Errorf("SEXTANT_PROVIDER: unknown provider %q (want %q or %q)",
			c.Provider, ProviderFake, ProviderAnthropic)
	}

	if c.Addr == "" {
		return fmt.Errorf("SEXTANT_ADDR: must not be empty")
	}

	// An override here decides where the API key is sent. Without a floor,
	// SEXTANT_PROVIDER_BASE_URL=http://attacker.example is accepted and the
	// credential leaves in cleartext to an arbitrary host.
	if err := validateEndpoint("SEXTANT_PROVIDER_BASE_URL", c.ProviderBaseURL, true); err != nil {
		return err
	}
	// The retriever is a sidecar reached over the private compose network, so
	// plaintext is expected there; it still has to be a well-formed absolute URL.
	if err := validateEndpoint("SEXTANT_RETRIEVER_URL", c.RetrieverURL, false); err != nil {
		return err
	}

	for _, o := range c.AllowedOrigins {
		if !httpx.IsValidOrigin(o) {
			return fmt.Errorf(
				"SEXTANT_ALLOWED_ORIGINS: %q is not a usable origin "+
					"(want scheme://host[:port], no trailing slash, no wildcard)", o)
		}
	}
	if c.ProviderTimeout <= 0 {
		return fmt.Errorf("SEXTANT_PROVIDER_TIMEOUT: must be positive, got %s", c.ProviderTimeout)
	}
	if c.MaxRepairDepth < 0 || c.MaxRepairDepth > MaxRepairDepthCeiling {
		return fmt.Errorf("SEXTANT_MAX_REPAIR_DEPTH: must be in [0,%d], got %d",
			MaxRepairDepthCeiling, c.MaxRepairDepth)
	}
	if c.BudgetUSD <= 0 || c.BudgetUSD > MaxBudgetUSDCeiling {
		return fmt.Errorf("SEXTANT_BUDGET_USD: must be in (0,%g], got %g",
			MaxBudgetUSDCeiling, c.BudgetUSD)
	}
	if c.WallClock <= 0 || c.WallClock > MaxWallClockCeiling {
		return fmt.Errorf("SEXTANT_WALL_CLOCK: must be in (0,%s], got %s",
			MaxWallClockCeiling, c.WallClock)
	}
	if c.RowLimit < 1 || c.RowLimit > MaxRowLimitCeiling {
		return fmt.Errorf("SEXTANT_ROW_LIMIT: must be in [1,%d], got %d",
			MaxRowLimitCeiling, c.RowLimit)
	}
	if c.StatementTimeout <= 0 || c.StatementTimeout > MaxStatementTimeoutCeiling {
		return fmt.Errorf("SEXTANT_STATEMENT_TIMEOUT: must be in (0,%s], got %s",
			MaxStatementTimeoutCeiling, c.StatementTimeout)
	}
	if c.ShutdownGrace <= 0 {
		return fmt.Errorf("SEXTANT_SHUTDOWN_GRACE: must be positive, got %s", c.ShutdownGrace)
	}
	if c.ParserTimeout <= 0 {
		return fmt.Errorf("SEXTANT_PARSER_TIMEOUT: must be positive, got %s", c.ParserTimeout)
	}
	if c.MaxConcurrentRuns < 1 {
		return fmt.Errorf("SEXTANT_MAX_CONCURRENT_RUNS: must be at least 1, got %d", c.MaxConcurrentRuns)
	}
	if c.RateLimitBurst < 1 {
		return fmt.Errorf("SEXTANT_RATE_LIMIT_BURST: must be at least 1, got %d", c.RateLimitBurst)
	}
	if c.RateLimitPerMinute <= 0 {
		return fmt.Errorf("SEXTANT_RATE_LIMIT_PER_MINUTE: must be positive, got %g", c.RateLimitPerMinute)
	}
	if c.MaxResultBytes < 1 || c.MaxResultBytes > MaxResultBytesCeiling {
		return fmt.Errorf("SEXTANT_MAX_RESULT_BYTES: must be in [1,%d], got %d",
			MaxResultBytesCeiling, c.MaxResultBytes)
	}
	return nil
}

// validateEndpoint checks that raw is an absolute URL with a host.
//
// When requireTLS is set, plaintext is rejected unless the host is loopback —
// the carve-out exists so the P2 record/replay harness can point at a local
// stub, and it is explicit rather than implied by leaving the field unchecked.
func validateEndpoint(key, raw string, requireTLS bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL %q: %w", key, raw, err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s: want an absolute http(s) URL, got %q", key, raw)
	}
	if requireTLS && u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("%s: refusing plaintext %q to a non-loopback host — "+
			"the provider API key would be sent in the clear", key, raw)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// parseOrigins splits a comma-separated allowlist, dropping blanks. An empty
// result means "no cross-origin browser client is allowed", which is the safe
// reading of an unset variable — never "allow everything".
func parseOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, raw, err)
	}
	return v, nil
}

func envFloat(key string, def float64) (float64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid number %q: %w", key, raw, err)
	}
	return v, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, raw, err)
	}
	return v, nil
}

func envBool(key string, def bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q: %w", key, raw, err)
	}
	return v, nil
}
