// Package httpx holds HTTP transport helpers that are not specific to any one
// endpoint.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// SSEStream writes Server-Sent Events to one client.
//
// The single-writer rule: an SSEStream is owned by exactly one goroutine and
// must never be written to concurrently. http.ResponseWriter is not safe for
// concurrent use, and the failure mode is interleaved half-frames rather than
// a clean panic. Producers fan into a channel; one goroutine drains it and
// calls Send. This mirrors the shape council uses for its WebSocket writer and
// is the same invariant for the same reason.
type SSEStream struct {
	w             http.ResponseWriter
	rc            *http.ResponseController
	writeDeadline time.Duration
	eventID       int
}

// SSE frames must not be buffered by an intermediary, or the "live trace
// timeline" is a timeline that arrives all at once at the end.
const (
	sseContentType = "text/event-stream"
	sseCacheHeader = "no-cache, no-transform"
)

// DefaultWriteDeadline bounds how long any single frame may block.
//
// It is applied automatically before every write, not left to the caller. A
// client that stops reading fills the TCP window and an in-flight Write blocks
// forever; context cancellation cannot interrupt it, so without a deadline one
// slow reader pins a goroutine, a file descriptor, and — once the agent loop
// writes traces — a slot on the single-connection SQLite pool. The per-run
// wall-clock cap does not help, because it only cancels a context.
const DefaultWriteDeadline = 10 * time.Second

// eventNamePattern is what an SSE event name may contain.
//
// The name is interpolated into the frame, so an unvalidated one carrying a
// newline ends the frame early and lets the remainder be read as a second,
// attacker-chosen frame — a client would validate and act on it exactly as if
// the server had sent it. Event names come from internal constants and
// classified failure kinds, some of which pass through model output on the way
// here, so this is an allowlist rather than an escape.
var eventNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ErrIllegalEventName is returned by Send for a name outside the allowlist.
var ErrIllegalEventName = errors.New("sse: illegal event name")

// NewSSEStream writes the SSE response headers and returns a stream ready for
// Send. It must be called before anything else writes to w.
//
// It fails when the ResponseWriter cannot be flushed, because an SSE stream
// that cannot flush is indistinguishable from a hung request from the client's
// side — better to fail the request outright than to serve a stream that never
// arrives.
func NewSSEStream(w http.ResponseWriter) (*SSEStream, error) {
	rc := http.NewResponseController(w)

	// Probed before the status line is written: once WriteHeader has run the
	// response is committed and the caller can no longer turn this into a 500.
	if _, ok := w.(http.Flusher); !ok {
		return nil, fmt.Errorf("sse: response writer is not flushable")
	}

	// Probed once, here, for the same reason as flushability: a stream whose
	// writes cannot be bounded is one a single slow reader can pin forever,
	// and construction is the last moment at which that can still become a
	// 500 rather than a half-served response. Real net/http writers support
	// this on HTTP/1 and HTTP/2; a test double that does not should say so.
	if err := rc.SetWriteDeadline(time.Now().Add(DefaultWriteDeadline)); err != nil {
		return nil, fmt.Errorf("sse: response writer does not support write deadlines: %w", err)
	}

	h := w.Header()
	h.Set("Content-Type", sseContentType)
	h.Set("Cache-Control", sseCacheHeader)
	h.Set("Connection", "keep-alive")
	// Nginx and several PaaS routers buffer proxied responses by default,
	// which defeats streaming entirely. This header opts out.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if err := rc.Flush(); err != nil {
		return nil, fmt.Errorf("sse: flushing response headers: %w", err)
	}
	return &SSEStream{w: w, rc: rc, writeDeadline: DefaultWriteDeadline}, nil
}

// SetWriteDeadlineDuration overrides the per-write deadline. Zero disables it,
// which should only ever be done in a test.
func (s *SSEStream) SetWriteDeadlineDuration(d time.Duration) {
	s.writeDeadline = d
}

// Send marshals v as JSON and writes it as one SSE frame of the given event
// type, then flushes.
//
// The context is checked first so a client that has gone away stops the
// producer at the next frame rather than at the next TCP timeout.
func (s *SSEStream) Send(ctx context.Context, event string, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event != "" && !eventNamePattern.MatchString(event) {
		return fmt.Errorf("%w: %q", ErrIllegalEventName, event)
	}

	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("sse: marshalling %s event: %w", event, err)
	}

	s.eventID++
	var b strings.Builder
	fmt.Fprintf(&b, "id: %d\n", s.eventID)
	if event != "" {
		fmt.Fprintf(&b, "event: %s\n", event)
	}
	// A newline inside the payload would end the frame early and desynchronize
	// the stream. json.Marshal never emits a raw newline, but the split keeps
	// that guarantee local rather than assumed.
	for _, line := range strings.Split(string(payload), "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")

	return s.write(b.String(), event)
}

// Heartbeat writes an SSE comment. Comments are ignored by clients but keep
// idle connections from being reaped by intermediate proxies.
func (s *SSEStream) Heartbeat(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.write(": heartbeat\n\n", "heartbeat")
}

// write refreshes the deadline and performs the actual write and flush.
//
// Refreshed per frame rather than set once for the stream: the deadline bounds
// how long a single frame may block, not how long a run may take. A run that
// legitimately streams for 30s must not be cut off at 10.
func (s *SSEStream) write(frame, what string) error {
	if s.writeDeadline > 0 {
		if err := s.rc.SetWriteDeadline(time.Now().Add(s.writeDeadline)); err != nil {
			return fmt.Errorf("sse: setting write deadline: %w", err)
		}
	}
	if _, err := s.w.Write([]byte(frame)); err != nil {
		return fmt.Errorf("sse: writing %s event: %w", what, err)
	}
	return s.rc.Flush()
}
