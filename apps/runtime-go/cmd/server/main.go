// Command server is the Sextant agent runtime.
//
// At P0 it serves health only: the question endpoint, the agent loop, and the
// SQL guard arrive in later phases. What it does establish is the shape
// everything else plugs into — structured logging, fail-fast config, a trace
// store, an origin allowlist, and a shutdown path that drains rather than
// drops.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/agent"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/api"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/config"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/dbreg"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/executor"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/guard"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/httpx"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/ratelimit"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/schema"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/trace"
)

const (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 120 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
	logger.Info("stopped cleanly")
}

// run owns the whole lifecycle and returns an error instead of exiting, so
// every failure path is testable and every deferred close actually runs.
func run(logger *slog.Logger) error {
	// Best-effort: a missing .env is normal in Docker and CI, where the real
	// environment is already populated. Values already in the environment win.
	if err := godotenv.Load(); err != nil {
		logger.Debug("no .env file loaded", "error", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration is invalid: %w", err)
	}
	// Safe to log wholesale: Config.LogValue redacts the API key.
	logger.Info("configuration loaded", "config", cfg)

	if cfg.Provider == config.ProviderFake && !cfg.ProviderAPIKey.IsZero() {
		logger.Warn("SEXTANT_PROVIDER_API_KEY is set but the provider is \"fake\"; " +
			"the credential is unused — unset it so it is not sitting in an " +
			"environment nobody is guarding")
	}

	// Rooted before any background goroutine starts, so everything below is
	// cancelled by the same signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	traceStore, err := trace.Open(cfg.TracePath)
	if err != nil {
		return fmt.Errorf("opening trace store at %s: %w", cfg.TracePath, err)
	}
	defer func() {
		if err := traceStore.Close(); err != nil {
			logger.Error("closing trace store", "error", err)
		}
	}()

	prov, err := buildProvider(cfg, logger)
	if err != nil {
		return fmt.Errorf("building provider: %w", err)
	}

	registry, err := dbreg.Parse(cfg.Databases)
	if err != nil {
		return fmt.Errorf("SEXTANT_DATABASES is invalid: %w", err)
	}
	if registry.Len() == 0 {
		// Not fatal: /healthz still serves and the drift gate still passes.
		// But every question will be refused, and that should be visible in
		// the log rather than discovered from a 404.
		logger.Warn("no databases configured; every question will be refused — set SEXTANT_DATABASES")
	}

	clk := clock.System{}
	loader := schema.NewLoader()
	handles, err := openDatabases(registry, loader)
	if err != nil {
		return err
	}
	defer func() {
		for slug, db := range handles {
			if err := db.Close(); err != nil {
				logger.Error("closing database", "database", slug, "error", err)
			}
		}
	}()

	parser, err := guard.NewHTTPParser(cfg.RetrieverURL, cfg.ParserTimeout, logger)
	if err != nil {
		return fmt.Errorf("building the SQL parser client: %w", err)
	}

	limiter, err := ratelimit.New(ratelimit.Config{
		Burst:     cfg.RateLimitBurst,
		PerMinute: cfg.RateLimitPerMinute,
		Clock:     clk,
	})
	if err != nil {
		return fmt.Errorf("building the rate limiter: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)

	// The question API is registered only when there is a database to serve.
	// A configured-but-unusable endpoint would answer 404 for a reason the
	// operator cannot see from the outside.
	if registry.Len() > 0 {
		srv, err := buildAPI(cfg, prov, parser, registry, loader, handles, limiter, clk, traceStore, logger)
		if err != nil {
			return err
		}
		srv.Routes(mux)
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		// Every route is wrapped, so a new endpoint cannot be added without
		// the allowlist applying to it.
		Handler:           httpx.CORS(cfg.AllowedOrigins, mux),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout: SSE responses are long-lived by design, and a
		// server-wide write deadline would cut every trace stream short. The
		// per-frame deadline in httpx.SSEStream bounds them instead.
	}

	// Buffered so the goroutine never blocks if shutdown wins the race.
	srvErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	// Distinguishing these two is the point: a bind failure must exit non-zero,
	// or every supervisor — Docker, compose, systemd, Kubernetes — reads a
	// server that never started as a run that finished successfully.
	select {
	case err := <-srvErr:
		if err != nil {
			return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining", "grace", cfg.ShutdownGrace.String())
	}

	// A fresh context: the one above may already be cancelled, and shutdown
	// needs its own budget to drain in-flight requests rather than dropping them.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	return nil
}

// buildProvider selects the LLM backend. The fake is the default, so a fresh
// clone runs with no credential and cannot make a paid call by accident.
func buildProvider(cfg config.Config, logger *slog.Logger) (provider.Provider, error) {
	switch cfg.Provider {
	case config.ProviderFake:
		return &provider.FakeProvider{}, nil
	case config.ProviderAnthropic:
		// The timeout is passed explicitly and the adapter refuses a
		// non-positive one. council's provider shipped without a timeout and a
		// hung upstream connection was then bounded only by the session
		// context; PLAN.md section 12 names it as the one defect not to repeat.
		p, err := provider.NewAnthropicProvider(provider.AnthropicConfig{
			APIKey:  cfg.ProviderAPIKey.Reveal(),
			BaseURL: cfg.ProviderBaseURL,
			Timeout: cfg.ProviderTimeout,
			Logger:  logger,
		})
		if err != nil {
			// Explicitly nil, not `return p, err`. A nil *AnthropicProvider
			// assigned to the Provider interface produces a NON-nil interface
			// holding a nil pointer, so `if prov != nil` would pass and the
			// first Stream call would panic. Returning the interface's own nil
			// is the only way to make that check mean what it reads as.
			return nil, err
		}
		return p, nil
	default:
		return nil, fmt.Errorf("unknown provider: %s", cfg.Provider)
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"service":"runtime-go","status":"ok"}`))
}

// openDatabases opens a read-only handle per configured database and registers
// it for schema introspection.
//
// Opened once at startup rather than per question: connecting is not free, and
// a database that cannot be opened should stop the server rather than fail the
// first question somebody asks.
func openDatabases(registry *dbreg.Registry, loader *schema.Loader) (map[string]*sql.DB, error) {
	handles := make(map[string]*sql.DB)
	for _, slug := range registry.Slugs() {
		db, err := registry.Lookup(slug)
		if err != nil {
			return nil, err
		}
		if db.Dialect != dbreg.DialectSQLite {
			// Postgres arrives with the dialect adapters at P3. Refusing at
			// startup beats accepting the config and failing every question.
			return nil, fmt.Errorf("database %q: dialect %q is not supported until P3", slug, db.Dialect)
		}
		handle, err := executor.OpenSQLiteReadOnly(db.DSN)
		if err != nil {
			return nil, fmt.Errorf("database %q: %w", slug, err)
		}
		handles[slug] = handle
		loader.Register(slug, handle)
	}
	return handles, nil
}

// buildAPI assembles the question server.
func buildAPI(
	cfg config.Config,
	prov provider.Provider,
	parser guard.Parser,
	registry *dbreg.Registry,
	loader *schema.Loader,
	handles map[string]*sql.DB,
	limiter *ratelimit.Limiter,
	clk clock.Clock,
	traceStore *trace.Store,
	logger *slog.Logger,
) (*api.Server, error) {
	// P1 serves one database, so one executor. P3's dialect adapters make this
	// a per-database map; the shape is deliberately left simple until then
	// rather than generalised against a requirement that does not exist yet.
	// One executor, so exactly one database may be configured. The API accepts
	// every registered slug and loads THAT database's schema, so a second
	// entry would mean a question planned against one schema and executed
	// against another — wrong answers, or reads of a database the caller never
	// named. Refusing at startup beats discovering it at query time.
	slugs := registry.Slugs()
	if len(slugs) > 1 {
		return nil, fmt.Errorf(
			"P1 serves a single database but %d are configured (%v); "+
				"per-database executors arrive with the dialect adapters at P3",
			len(slugs), slugs)
	}
	exec, err := executor.New(handles[slugs[0]], clk, cfg.MaxResultBytes)
	if err != nil {
		return nil, fmt.Errorf("building the executor: %w", err)
	}

	ag, err := agent.New(agent.Deps{
		Provider:   prov,
		Parser:     parser,
		Executor:   exec,
		Clock:      clk,
		Logger:     logger,
		CheapModel: cfg.CheapModel,
	})
	if err != nil {
		return nil, fmt.Errorf("building the agent: %w", err)
	}

	return api.New(api.Deps{
		Agent:             ag,
		Trace:             traceStore,
		Databases:         registry,
		Schemas:           loader,
		Limiter:           limiter,
		Clock:             clk,
		Logger:            logger,
		TrustProxy:        cfg.TrustProxy,
		MaxConcurrentRuns: cfg.MaxConcurrentRuns,
		MaxRepairDepth:    cfg.MaxRepairDepth,
		BudgetUSD:         cfg.BudgetUSD,
		WallClock:         cfg.WallClock,
		RowLimit:          cfg.RowLimit,
		StatementTimeout:  cfg.StatementTimeout,
	})
}
