// Package api serves the question endpoint and the run stream.
//
// Two endpoints, and the split between them is deliberate. EventSource can
// only issue a GET, so a question cannot be posted and streamed on one
// connection:
//
//	POST /v1/questions        validate, write the run row, return {run_id}
//	GET  /v1/runs/{id}/events start the loop and stream every event
//
// The run starts on the GET, not on the POST. That means no event can be
// emitted before a reader exists, so there is no buffer to size, no replay to
// implement, and no race in which the first frames are produced before anyone
// is listening.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/agent"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/dbreg"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/httpx"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/ratelimit"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/schema"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/trace"
)

// Runner is the agent, as an interface.
//
// An interface so this package's tests drive a fake: the real loop is an open
// stub that panics, and handler tests must stay green while it is. That is what
// keeps the CI test job red on exactly the two packages whose stubs are
// unwritten instead of on everything that transitively imports them.
type Runner interface {
	Run(ctx context.Context, in agent.RunInput, emit agent.Emit) (agent.Result, error)
}

// TraceStore is the persistence this package needs.
type TraceStore interface {
	StartRun(ctx context.Context, run trace.Run) error
	FinishRun(ctx context.Context, runID, outcome string, at time.Time) error
	AppendEvent(ctx context.Context, runID string, ev trace.Event) error
	AppendCost(ctx context.Context, runID string, e trace.CostEntry) error
	LoadRun(ctx context.Context, runID string) (trace.Run, error)
	ClaimRun(ctx context.Context, runID string) (bool, error)
}

// SchemaLoader resolves a database to its introspected schema.
type SchemaLoader interface {
	Load(ctx context.Context, db dbreg.Database) (schema.Schema, error)
}

// Deps are what the server needs.
type Deps struct {
	Agent     Runner
	Trace     TraceStore
	Databases *dbreg.Registry
	Schemas   SchemaLoader
	Limiter   *ratelimit.Limiter
	Clock     clock.Clock
	Logger    *slog.Logger

	// TrustProxy decides whether X-Forwarded-For is believed when identifying
	// a client for rate limiting.
	TrustProxy bool
	// MaxConcurrentRuns bounds how many loops may be in flight at once.
	MaxConcurrentRuns int

	// Server ceilings. A request may ask for less, never more.
	MaxRepairDepth   int
	BudgetUSD        float64
	WallClock        time.Duration
	RowLimit         int
	StatementTimeout time.Duration
}

// Server serves the question API.
type Server struct {
	deps Deps
	// slots bounds concurrent runs. A buffered channel rather than a
	// semaphore type: acquiring it is a select that can also observe
	// ctx.Done(), so a client that goes away while queued does not hold a slot
	// it will never use.
	slots chan struct{}
}

// New builds a server.
func New(deps Deps) (*Server, error) {
	switch {
	case deps.Agent == nil:
		return nil, errors.New("api: nil agent")
	case deps.Trace == nil:
		return nil, errors.New("api: nil trace store")
	case deps.Databases == nil:
		return nil, errors.New("api: nil database registry")
	case deps.Schemas == nil:
		return nil, errors.New("api: nil schema loader")
	case deps.Limiter == nil:
		return nil, errors.New("api: nil rate limiter")
	case deps.Clock == nil:
		return nil, errors.New("api: nil clock")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.MaxConcurrentRuns <= 0 {
		return nil, errors.New("api: max concurrent runs must be positive")
	}
	return &Server{deps: deps, slots: make(chan struct{}, deps.MaxConcurrentRuns)}, nil
}

// Routes registers the endpoints on a mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/questions", s.handleAsk)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.handleStream)
}

// maxQuestionBody bounds the request body.
//
// question_request.v1 caps the question at 2000 characters and the evidence at
// 4000, so a conforming request is small. This bounds what an unconforming one
// can make the server read before it finds that out.
const maxQuestionBody = 64 * 1024

// handleAsk validates a question and reserves a run for it.
//
// It starts nothing. The loop begins when the stream is opened, so this
// endpoint is cheap, makes no provider call, and cannot spend money — which is
// also why the rate limit is applied here, where the cost of refusing is
// lowest.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	// A text/plain POST is a CORS "simple request" and needs no preflight, so
	// without this any page a visitor opens could create runs on a reachable
	// instance without the origin allowlist ever being consulted. Requiring
	// application/json forces a preflight and puts the endpoint back behind
	// the allowlist.
	if ct := r.Header.Get("Content-Type"); !isJSONContentType(ct) {
		writeError(w, http.StatusUnsupportedMediaType, "expected a JSON request body")
		return
	}
	if !s.deps.Limiter.Allow(ratelimit.ClientKey(r, s.deps.TrustProxy)) {
		writeError(w, http.StatusTooManyRequests, "too many questions; slow down")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxQuestionBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
		return
	}

	// Validated at the boundary, before anything acts on it.
	if err := contracts.Validate(contracts.QuestionRequestV1, raw); err != nil {
		var invalid contracts.ValidationError
		if errors.As(err, &invalid) {
			// Error() quotes the offending instance values — which for a
			// request means echoing the client's own input back. Log that;
			// return the safe form.
			s.deps.Logger.Info("rejected a question request", "error", invalid.Error())
			writeError(w, http.StatusBadRequest, invalid.SafeMessage())
			return
		}
		writeError(w, http.StatusBadRequest, "the request could not be validated")
		return
	}

	var req gen.QuestionRequestV1
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "the request could not be read")
		return
	}

	// The slug is a registry key and never a path.
	db, err := s.deps.Databases.Lookup(req.Database)
	if err != nil {
		var unknown dbreg.ErrUnknownDatabase
		if errors.As(err, &unknown) {
			s.deps.Logger.Info("question named an unconfigured database", "error", unknown.Error())
			writeError(w, http.StatusNotFound, unknown.SafeMessage())
			return
		}
		writeError(w, http.StatusNotFound, "no such database")
		return
	}

	runID, err := newRunID()
	if err != nil {
		s.deps.Logger.Error("generating a run id", "error", err)
		writeError(w, http.StatusInternalServerError, "could not start a run")
		return
	}

	run := trace.Run{
		ID:       runID,
		Question: req.Question,
		Database: db.Slug,
		// Carried across the POST/GET split. BIRD supplies evidence per item
		// and prompt.go calls it "often the only thing that makes a question
		// answerable" — dropping it here would make that comment describe a
		// value that is always empty.
		Evidence:  req.Evidence,
		StartedAt: s.deps.Clock.Now(),
	}
	if err := s.deps.Trace.StartRun(r.Context(), run); err != nil {
		s.deps.Logger.Error("recording run", "run_id", runID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not start a run")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"run_id": runID,
		"events": "/v1/runs/" + runID + "/events",
	})
}

// handleStream starts the run and writes its trace.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	run, err := s.deps.Trace.LoadRun(r.Context(), runID)
	if err != nil {
		// Deliberately the same answer for "no such run" and "a run belonging
		// to someone else": run ids are unguessable, and distinguishing the
		// two would turn that into an oracle.
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	if run.Outcome != "" {
		// Covers both a finished run and one already in flight. P1 has no
		// replay: streaming again would mean RUNNING again, and spending money
		// a second time for a question already asked.
		writeError(w, http.StatusConflict, "this run has already been started")
		return
	}

	db, err := s.deps.Databases.Lookup(run.Database)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such database")
		return
	}

	// Claim the run atomically. The LoadRun check above is a courtesy that
	// gives a clean 409 on the common case; this is the one that actually
	// holds, because two concurrent requests can both pass that check and only
	// one can win this UPDATE. Without it, one POSTed question can be streamed
	// N times concurrently and billed N times.
	claimed, err := s.deps.Trace.ClaimRun(r.Context(), runID)
	if err != nil {
		s.deps.Logger.Error("claiming run", "run_id", runID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not start a run")
		return
	}
	if !claimed {
		writeError(w, http.StatusConflict, "this run has already been started")
		return
	}

	// Acquire a run slot before committing to a stream. Bounding concurrent
	// loops bounds concurrent provider connections, which is the thing that
	// actually costs money. Non-blocking: a queued client would hold the claim
	// while waiting, and a 503 it can retry is better than a slot it cannot.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeError(w, http.StatusServiceUnavailable, "too many runs in flight; try again shortly")
		// The claim has to come back, or a 503 would burn the run permanently.
		s.releaseClaim(runID)
		return
	}

	sch, err := s.deps.Schemas.Load(r.Context(), db)
	if err != nil {
		s.deps.Logger.Error("loading schema", "database", db.Slug, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the database schema")
		s.releaseClaim(runID)
		return
	}

	// Everything above may still answer with a status code. From here the
	// response is committed to being a stream.
	stream, err := httpx.NewSSEStream(w)
	if err != nil {
		// NewSSEStream can fail after WriteHeader(200) — the flush path — at
		// which point the response is committed and writeError would only log
		// "superfluous WriteHeader" while the client reads a 200 carrying an
		// error body. Nothing useful can be sent; log it and let the empty
		// response stand.
		s.deps.Logger.Error("opening the event stream", "run_id", runID, "error", err)
		s.releaseClaim(runID)
		return
	}

	// The wall-clock cap is a property of the run, so it belongs on the
	// context every step inherits rather than being checked only where
	// somebody remembered to.
	ctx, cancel := context.WithTimeout(r.Context(), s.deps.WallClock)
	defer cancel()

	s.streamRun(ctx, cancel, stream, run, agent.RunInput{
		RunID:    run.ID,
		Question: run.Question,
		Evidence: run.Evidence,
		Schema:   sch,
		Dialect:  dialectOf(db),
		Caps: agent.Caps{
			MaxRepairDepth: s.deps.MaxRepairDepth,
			MaxUSD:         s.deps.BudgetUSD,
			WallClock:      s.deps.WallClock,
		},
		RowLimit:         s.deps.RowLimit,
		StatementTimeout: s.deps.StatementTimeout,
	})
}

// releaseClaim returns a claimed run to the unstarted state.
//
// Called on every path that claims and then cannot proceed. Without it a 503
// or a failed schema load would burn the run permanently: the claim marks it
// started, and nothing would ever finish it.
func (s *Server) releaseClaim(runID string) {
	// A fresh context: the request's may already be cancelled, and this write
	// is what keeps the run usable.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), persistTimeout)
	defer cancel()
	if err := s.deps.Trace.FinishRun(ctx, runID, "", s.deps.Clock.Now()); err != nil {
		s.deps.Logger.Error("releasing run claim", "run_id", runID, "error", err)
	}
}

func dialectOf(db dbreg.Database) gen.SqlPlanV1Dialect {
	if db.Dialect == dbreg.DialectPostgres {
		return gen.SqlPlanV1DialectPostgres
	}
	return gen.SqlPlanV1DialectSqlite
}

// newRunID returns an unguessable run identifier.
//
// Unguessable matters: a run id is the only thing protecting one client's
// stream from another, so a sequential or timestamp-derived id would let
// anyone enumerate other people's questions and answers.
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "r_" + hex.EncodeToString(b[:]), nil
}

// isJSONContentType reports whether a Content-Type header names JSON.
//
// Tolerant of parameters (`application/json; charset=utf-8`) because clients
// legitimately send them, and of case because the header is case-insensitive.
func isJSONContentType(header string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// writeError returns a JSON error with a message safe for a client.
//
// Every caller passes a fixed string or an already-sanitized one. Nothing that
// originated in a provider, a driver, or the request itself reaches here.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
