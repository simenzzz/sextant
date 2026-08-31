package api

import (
	"context"
	"encoding/json"
	"runtime/debug"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/agent"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/httpx"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/trace"
)

// SSE event names. Allowlisted at the writer, and these are the whole set.
//
// Trace events go on the default name so an EventSource's `onmessage` receives
// them; the result and the ledger are named because they are different
// contracts and the client validates each against its own schema.
const (
	eventTrace      = "" // default `message` event
	eventResultSet  = "result_set"
	eventCostLedger = "cost_ledger"
)

// emitBuffer bounds how many events may queue between the loop and the writer.
//
// Buffered so a burst of fast transitions does not block the loop on a slow
// reader, small enough that the queue cannot become an unbounded copy of the
// run. A run is bounded by construction, so this is generous.
const emitBuffer = 64

// persistTimeout bounds a write that must outlive the run's own context.
//
// Short: these run after the answer is already on the wire, and a slow disk
// must not hold a run slot open. Bounded rather than unbounded so shutdown
// still drains.
const persistTimeout = 5 * time.Second

// streamRun executes one run and writes its whole stream.
//
// The single-writer discipline lives here. The agent loop runs on its own
// goroutine and produces events into a channel; THIS goroutine is the only one
// that ever touches the SSEStream. http.ResponseWriter is not safe for
// concurrent use and the failure mode is interleaved half-frames rather than a
// clean panic, so "one writer" is not a style preference — it is the
// difference between a stream a client can parse and one it cannot.
//
// The same goroutine also persists each event, which keeps the stored trace and
// the streamed one identical by construction rather than by two code paths
// agreeing.
func (s *Server) streamRun(
	ctx context.Context,
	cancel context.CancelFunc,
	stream *httpx.SSEStream,
	run trace.Run,
	in agent.RunInput,
) {
	// Persistence outlives the run's context, deliberately.
	//
	// ctx carries both the wall-clock cap and the client's connection, so it is
	// already cancelled in exactly the two cases where recording matters most:
	// a run that hit its deadline, and a reader that went away. Writing the
	// trace, the ledger, and the outcome through it meant those writes failed
	// silently — leaving the run row with an empty outcome, which the stream
	// handler reads as "not started yet" and will happily run AGAIN. That turns
	// the wall-clock cap into an unbounded spend loop, and it makes
	// `deadline_exceeded` a recorded outcome that is never actually recorded.
	persistCtx, endPersist := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer endPersist()

	events := make(chan gen.TraceEventV1, emitBuffer)

	// step and elapsed are stamped by the writer, not by the loop. They are
	// properties of the stream — a monotonic index and a duration since the
	// run began — and asking the loop to track them would be asking it to get
	// one wrong.
	started := s.deps.Clock.Now()
	var step int

	done := make(chan agent.Result, 1)
	go func() {
		defer close(events)
		// A panic here would otherwise kill the PROCESS. net/http recovers
		// panics on the goroutine running a handler, but not on one the
		// handler spawned — and the loop runs on a spawned goroutine because
		// this one has to stay free to drain and write.
		//
		// This is not defensive padding. The loop is where the agent's own
		// logic lives, and one question's bug taking down the server for every
		// other client is a far worse failure than that question ending badly.
		// The panic becomes a recorded error outcome, and the stack goes to the
		// log where it can be read.
		defer func() {
			if r := recover(); r != nil {
				s.deps.Logger.Error("agent run panicked",
					"run_id", run.ID, "panic", r, "stack", string(debug.Stack()))
				done <- agent.Result{
					Outcome: agent.OutcomeError,
					Ledger:  agent.NewLedger(run.ID).Document(),
				}
			}
		}()

		result, err := s.deps.Agent.Run(ctx, in, func(ev gen.TraceEventV1) {
			select {
			case <-ctx.Done():
			case events <- ev:
			}
		})
		if err != nil {
			s.deps.Logger.Error("agent run failed", "run_id", run.ID, "error", err)
		}
		done <- result
	}()

	for ev := range events {
		ev.Step = step
		ev.ElapsedMs = clock.ElapsedMS(s.deps.Clock, started)
		step++

		// A token fragment is streamed but not stored. It is a progress
		// indicator whose content is already recorded in full by the
		// `generated` event, and a long generation would otherwise write one
		// row per token — thousands of rows per run, for a trace nobody reads
		// fragment by fragment.
		if !isDeltaOnly(ev) {
			s.persistEvent(persistCtx, run.ID, ev)
		}

		if err := s.send(ctx, stream, eventTrace, ev, contracts.TraceEventV1); err != nil {
			// The client went away or a frame hit its write deadline. Cancel
			// the run rather than only stopping the writer: without this the
			// producer fills the buffer, blocks in emit, and this goroutine
			// blocks on <-done — pinning a run slot for the whole wall clock.
			// Eight unread connections would then make every other client see
			// 503 for as long as the cap allows.
			s.deps.Logger.Info("trace stream ended early", "run_id", run.ID, "error", err)
			cancel()
			break
		}
	}

	// Drain whatever the loop still emits, so a producer that raced the cancel
	// cannot block forever on a send nobody receives.
	for range events {
	}

	result := <-done
	s.finish(persistCtx, stream, run, result)
}

// finish writes the terminal frames and closes out the run.
//
// persistCtx outlives the run's own context: see streamRun. The SSE sends use
// it too — a run that ended at its deadline still has an answer worth
// delivering, and the frame write has its own deadline anyway.
func (s *Server) finish(persistCtx context.Context, stream *httpx.SSEStream, run trace.Run, result agent.Result) {
	ctx := persistCtx
	if result.Rows != nil {
		if err := s.send(ctx, stream, eventResultSet, result.Rows, contracts.ResultSetV1); err != nil {
			s.deps.Logger.Info("result frame not delivered", "run_id", run.ID, "error", err)
		}
	}
	if err := s.send(ctx, stream, eventCostLedger, result.Ledger, contracts.CostLedgerV1); err != nil {
		s.deps.Logger.Info("ledger frame not delivered", "run_id", run.ID, "error", err)
	}

	s.persistLedger(ctx, run.ID, result)

	outcome := string(result.Outcome)
	if outcome == "" {
		outcome = string(agent.OutcomeError)
	}
	if err := s.deps.Trace.FinishRun(ctx, run.ID, outcome, s.deps.Clock.Now()); err != nil {
		s.deps.Logger.Error("recording run outcome", "run_id", run.ID, "error", err)
	}
}

// send validates a document against its contract, then writes it as one frame.
//
// Validated on the way OUT as well as in. The browser revalidates and drops
// anything that fails, so an event this runtime emits that violates its own
// contract disappears silently and looks to a user like a stalled stream. This
// is where that becomes a server-side log line instead.
func (s *Server) send(
	ctx context.Context,
	stream *httpx.SSEStream,
	name string,
	doc any,
	contract contracts.Name,
) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		s.deps.Logger.Error("marshalling outbound frame", "contract", contract, "error", err)
		return err
	}
	if err := contracts.Validate(contract, raw); err != nil {
		// Logged and dropped rather than sent: a nonconforming frame would be
		// rejected by the client anyway, and sending it would put a
		// desynchronized stream in front of a user.
		s.deps.Logger.Error("refusing to emit a nonconforming frame",
			"contract", contract, "error", err)
		return err
	}
	return stream.Send(ctx, name, doc)
}

func (s *Server) persistEvent(ctx context.Context, runID string, ev gen.TraceEventV1) {
	payload, err := json.Marshal(ev)
	if err != nil {
		s.deps.Logger.Error("marshalling trace event", "run_id", runID, "error", err)
		return
	}
	// A persistence failure must not end the run. The client is watching a
	// live stream; losing the stored copy degrades later inspection but does
	// not make the answer in front of them wrong.
	if err := s.deps.Trace.AppendEvent(ctx, runID, trace.Event{
		Step: ev.Step, Type: string(ev.Type), ElapsedMS: ev.ElapsedMs, Payload: payload,
	}); err != nil {
		s.deps.Logger.Error("persisting trace event", "run_id", runID, "step", ev.Step, "error", err)
	}
}

func (s *Server) persistLedger(ctx context.Context, runID string, result agent.Result) {
	for _, e := range result.Ledger.Entries {
		if err := s.deps.Trace.AppendCost(ctx, runID, trace.CostEntry{
			Step: e.Step, Model: e.Model,
			TokensIn: e.TokensIn, TokensOut: e.TokensOut,
			USD: e.Usd, MS: e.Ms,
			CacheHit: e.CacheHit, Escalated: e.Escalated, RepairDepth: e.RepairDepth,
		}); err != nil {
			s.deps.Logger.Error("persisting cost entry", "run_id", runID, "step", e.Step, "error", err)
		}
	}
}

// isDeltaOnly reports whether an event carries nothing but a token fragment.
//
// Narrow on purpose: only a `generating` event whose sole payload is `delta`
// is skipped by the trace store. The `generating` event that opens the phase
// carries the model and no delta, so it is stored.
func isDeltaOnly(ev gen.TraceEventV1) bool {
	return ev.Type == gen.TraceEventV1TypeGenerating && ev.Delta != nil
}
