package api

import (
	"context"
	"encoding/json"

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
	stream *httpx.SSEStream,
	run trace.Run,
	in agent.RunInput,
) {
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

		s.persistEvent(ctx, run.ID, ev)

		if err := s.send(ctx, stream, eventTrace, ev, contracts.TraceEventV1); err != nil {
			// The client went away or the write deadline expired. Stop
			// writing, but let the loop finish and be recorded: a run whose
			// reader disconnected still happened and still cost money.
			s.deps.Logger.Info("trace stream ended early", "run_id", run.ID, "error", err)
			break
		}
	}

	result := <-done
	s.finish(ctx, stream, run, result)
}

// finish writes the terminal frames and closes out the run.
func (s *Server) finish(ctx context.Context, stream *httpx.SSEStream, run trace.Run, result agent.Result) {
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
