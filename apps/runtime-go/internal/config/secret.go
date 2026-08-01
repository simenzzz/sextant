package config

import "log/slog"

// redacted is what a Secret renders as, everywhere.
const redacted = "[REDACTED]"

// Secret is a credential that cannot be printed by accident.
//
// It exists because the alternative — a plain string field — relies on every
// present and future log call remembering to omit it. One
// `logger.Debug("cfg", "config", cfg)` during a debugging session is enough to
// write an API key into structured logs, which is exactly where log shippers,
// crash reporters, and pasted `docker logs` output go. Making the type refuse
// to render is cheaper than auditing every call site forever.
//
// Reveal is the single, greppable way to get the real value.
type Secret string

// String implements fmt.Stringer, covering %s, %v, and print-family calls.
func (Secret) String() string { return redacted }

// LogValue implements slog.LogValuer, covering structured logging.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON covers anything that serializes the config, including the JSON
// slog handler this service uses.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Reveal returns the underlying credential. Call it only where the value is
// actually used — building a provider client — never to log or format it.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is unset.
func (s Secret) IsZero() bool { return s == "" }

// LogValue keeps the whole Config safe to hand to a logger, so
// `logger.Info("config", "cfg", cfg)` cannot leak the key even by accident.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", c.Addr),
		slog.Any("allowed_origins", c.AllowedOrigins),
		slog.Bool("trust_proxy", c.TrustProxy),
		slog.String("retriever_url", c.RetrieverURL),
		slog.String("trace_path", c.TracePath),
		slog.String("provider", c.Provider),
		slog.String("provider_api_key", redacted),
		slog.String("provider_base_url", c.ProviderBaseURL),
		slog.Duration("provider_timeout", c.ProviderTimeout),
		slog.String("cheap_model", c.CheapModel),
		slog.String("strong_model", c.StrongModel),
		slog.Int("max_repair_depth", c.MaxRepairDepth),
		slog.Float64("budget_usd", c.BudgetUSD),
		slog.Duration("wall_clock", c.WallClock),
		slog.Int("row_limit", c.RowLimit),
		slog.Duration("statement_timeout", c.StatementTimeout),
		slog.Duration("shutdown_grace", c.ShutdownGrace),
	)
}
