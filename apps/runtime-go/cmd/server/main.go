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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/config"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/httpx"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
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
	_ = prov // wired into the agent loop later in P1

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)

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
