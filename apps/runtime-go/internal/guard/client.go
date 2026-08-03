package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// Parser turns a candidate statement into a parse summary.
//
// An interface so the guard's own tests drive a fake and never a live sidecar,
// and so P3 can swap a pure-Go parser in behind it without the policy changing
// at all. parse_summary.v1 is what makes that swap a one-file change.
type Parser interface {
	Parse(ctx context.Context, sql string, dialect gen.SqlPlanV1Dialect, limitCap int) (gen.ParseSummaryV1, error)
}

// HTTPParser calls the retriever sidecar's POST /v1/parse.
type HTTPParser struct {
	baseURL string
	client  *http.Client
	logger  *slog.Logger
}

// maxParseResponseBytes bounds what the sidecar may return.
//
// The sidecar is ours, but it is a separate process on a network, and a
// runaway or wedged one must not be able to make the runtime read an unbounded
// body into memory on the request path.
const maxParseResponseBytes = 1 << 20

// NewHTTPParser builds a parser client.
//
// The timeout is required and refused if non-positive, for the same reason the
// provider's is: this is a network call on the hot path of every question, and
// an unbounded one would hang a run past every cap meant to bound it.
func NewHTTPParser(baseURL string, timeout time.Duration, logger *slog.Logger) (*HTTPParser, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("guard: parser base URL is empty")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("guard: parser requires a positive timeout")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &HTTPParser{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
		logger:  logger,
	}, nil
}

// Parse asks the sidecar to summarize one statement.
//
// Every failure path returns an error rather than a usable summary. That is
// what "fails closed" means concretely: the sidecar being unreachable must
// look nothing like the sidecar approving a statement, and there is no return
// value from this function that a caller could mistake for approval.
func (p *HTTPParser) Parse(
	ctx context.Context,
	sql string,
	dialect gen.SqlPlanV1Dialect,
	limitCap int,
) (gen.ParseSummaryV1, error) {
	body, err := json.Marshal(map[string]any{
		"sql":       sql,
		"dialect":   string(dialect),
		"limit_cap": limitCap,
	})
	if err != nil {
		return gen.ParseSummaryV1{}, fmt.Errorf("guard: building parse request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/parse", bytes.NewReader(body))
	if err != nil {
		return gen.ParseSummaryV1{}, fmt.Errorf("guard: building parse request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		// The detail names the sidecar's address. Logged, not returned.
		p.logger.Error("guard: parser unreachable", "error", err)
		return gen.ParseSummaryV1{}, fmt.Errorf("guard: the SQL parser is unavailable")
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxParseResponseBytes))
	if err != nil {
		p.logger.Error("guard: reading parser response", "error", err)
		return gen.ParseSummaryV1{}, fmt.Errorf("guard: the SQL parser returned an unreadable response")
	}

	if resp.StatusCode != http.StatusOK {
		// A non-200 means the runtime called the service wrongly, or the
		// service is broken. Neither is something a repair can fix, and the
		// body is the sidecar's own diagnostic — logged, never forwarded.
		p.logger.Error("guard: parser rejected the request",
			"status", resp.StatusCode, "body", string(raw))
		return gen.ParseSummaryV1{}, fmt.Errorf("guard: the SQL parser rejected the request")
	}

	// Validated on the way in. The guard reads specific fields and fails
	// closed on what it cannot establish, so a malformed summary must be
	// refused here rather than read field by field and quietly half-trusted.
	if err := contracts.Validate(contracts.ParseSummaryV1, raw); err != nil {
		p.logger.Error("guard: parser returned a nonconforming summary", "error", err)
		return gen.ParseSummaryV1{}, fmt.Errorf("guard: the SQL parser returned an unusable summary")
	}

	var summary gen.ParseSummaryV1
	if err := json.Unmarshal(raw, &summary); err != nil {
		p.logger.Error("guard: decoding parse summary", "error", err)
		return gen.ParseSummaryV1{}, fmt.Errorf("guard: the SQL parser returned an unusable summary")
	}
	return summary, nil
}
