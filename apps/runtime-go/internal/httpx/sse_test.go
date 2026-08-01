package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewSSEStreamSetsStreamingHeaders(t *testing.T) {
	rec := newRecorder()
	if _, err := NewSSEStream(rec); err != nil {
		t.Fatalf("NewSSEStream: %v", err)
	}

	tests := []struct {
		header string
		want   string
	}{
		{"Content-Type", sseContentType},
		{"Cache-Control", sseCacheHeader},
		// Without this an nginx or PaaS router buffers the whole response and
		// the "live" trace timeline arrives in one lump at the end.
		{"X-Accel-Buffering", "no"},
	}
	for _, tt := range tests {
		if got := rec.Header().Get(tt.header); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.header, got, tt.want)
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSendFramesEvents(t *testing.T) {
	rec := newRecorder()
	s, err := NewSSEStream(rec)
	if err != nil {
		t.Fatalf("NewSSEStream: %v", err)
	}

	type payload struct {
		Type string `json:"type"`
		Step int    `json:"step"`
	}
	ctx := context.Background()
	if err := s.Send(ctx, "trace", payload{Type: "run_started", Step: 0}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(ctx, "trace", payload{Type: "answered", Step: 1}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"id: 1\n", "id: 2\n",
		"event: trace\n",
		`data: {"type":"run_started","step":0}` + "\n",
		`data: {"type":"answered","step":1}` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- got ---\n%s", want, body)
		}
	}

	// Two events means two frames, each terminated by a blank line.
	if got := strings.Count(body, "\n\n"); got != 2 {
		t.Errorf("frame terminators = %d, want 2\n--- got ---\n%s", got, body)
	}
}

// A cancelled request must stop the producer at the next frame rather than
// writing into a connection nobody is reading.
func TestSendStopsOnCancelledContext(t *testing.T) {
	rec := newRecorder()
	s, err := NewSSEStream(rec)
	if err != nil {
		t.Fatalf("NewSSEStream: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Send(ctx, "trace", map[string]string{"type": "run_started"}); err == nil {
		t.Fatal("Send() = nil error on a cancelled context, want the context error")
	}
	if body := rec.Body.String(); strings.Contains(body, "data:") {
		t.Errorf("Send wrote a frame despite cancellation: %q", body)
	}
	if err := s.Heartbeat(ctx); err == nil {
		t.Fatal("Heartbeat() = nil error on a cancelled context, want the context error")
	}
}

func TestSendRejectsUnmarshalablePayload(t *testing.T) {
	rec := newRecorder()
	s, err := NewSSEStream(rec)
	if err != nil {
		t.Fatalf("NewSSEStream: %v", err)
	}

	// A channel cannot be marshalled. The stream must report the error rather
	// than emitting a half-written frame.
	err = s.Send(context.Background(), "trace", make(chan int))
	if err == nil {
		t.Fatal("Send() = nil error for an unmarshalable payload")
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Error("Send emitted a data line for a payload it could not marshal")
	}
}

func TestHeartbeatWritesComment(t *testing.T) {
	rec := newRecorder()
	s, err := NewSSEStream(rec)
	if err != nil {
		t.Fatalf("NewSSEStream: %v", err)
	}
	if err := s.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := rec.Body.String(); !strings.HasPrefix(got, ": heartbeat\n\n") {
		t.Errorf("body = %q, want an SSE comment frame", got)
	}
}

// An event name is interpolated into the frame. A name carrying a newline
// would end the frame early and let the remainder be read as a second,
// attacker-chosen frame that the client would validate and act on.
func TestSendRejectsIllegalEventNames(t *testing.T) {
	injection := "trace\n\ndata: {\"schema\":\"trace_event.v1\",\"type\":\"answered\"," +
		"\"step\":0,\"elapsed_ms\":0}\n"

	tests := []struct {
		name  string
		event string
	}{
		{"frame injection via newline", injection},
		{"carriage return", "trace\rdata: x"},
		{"space", "trace event"},
		{"colon", "trace:evil"},
		{"over length", strings.Repeat("a", 65)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder()
			s, err := NewSSEStream(rec)
			if err != nil {
				t.Fatalf("NewSSEStream: %v", err)
			}

			err = s.Send(context.Background(), tt.event, map[string]string{"a": "b"})
			if !errors.Is(err, ErrIllegalEventName) {
				t.Fatalf("Send() error = %v, want ErrIllegalEventName", err)
			}
			if body := rec.Body.String(); strings.Contains(body, "data:") {
				t.Errorf("a frame was written for a rejected event name: %q", body)
			}
		})
	}
}

func TestSendAcceptsLegalEventNames(t *testing.T) {
	for _, event := range []string{"trace", "trace_event", "cost.ledger", "run-started", ""} {
		t.Run("name="+event, func(t *testing.T) {
			rec := newRecorder()
			s, err := NewSSEStream(rec)
			if err != nil {
				t.Fatalf("NewSSEStream: %v", err)
			}
			if err := s.Send(context.Background(), event, map[string]int{"step": 0}); err != nil {
				t.Fatalf("Send(%q) = %v, want nil", event, err)
			}
		})
	}
}

// Without a deadline an in-flight Write blocks forever once a client stops
// reading, and context cancellation cannot interrupt it — one slow reader
// then pins a goroutine and a connection indefinitely.
func TestStreamAppliesAWriteDeadlineByDefault(t *testing.T) {
	rec := newRecorder()
	s, err := NewSSEStream(rec)
	if err != nil {
		t.Fatalf("NewSSEStream: %v", err)
	}
	if s.writeDeadline != DefaultWriteDeadline {
		t.Errorf("writeDeadline = %s, want %s", s.writeDeadline, DefaultWriteDeadline)
	}

	if err := s.Send(context.Background(), "trace", map[string]int{"step": 0}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.deadlines < 2 {
		t.Errorf("SetWriteDeadline called %d times, want one at construction plus one per Send",
			rec.deadlines)
	}

	before := rec.deadlines
	if err := s.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if rec.deadlines == before {
		t.Error("Heartbeat wrote without setting a write deadline")
	}
}

// deadlineRecorder is an httptest.ResponseRecorder that also supports write
// deadlines, as every real net/http writer does. Plain ResponseRecorder does
// not implement the method, and NewSSEStream now refuses such a writer — so
// this is the minimum realistic double, not a convenience.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines int
}

func newRecorder() *deadlineRecorder {
	return &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (d *deadlineRecorder) SetWriteDeadline(time.Time) error {
	d.deadlines++
	return nil
}

// A writer that cannot be bounded is one a single slow reader can pin forever.
func TestNewSSEStreamRejectsWriterWithoutDeadlineSupport(t *testing.T) {
	if _, err := NewSSEStream(httptest.NewRecorder()); err == nil {
		t.Fatal("NewSSEStream(no deadline support) = nil error, want a failure")
	}
}

// A writer that cannot be flushed must be rejected before the status line is
// written, or the caller can no longer turn the failure into a 500.
func TestNewSSEStreamRejectsUnflushableWriter(t *testing.T) {
	w := &unflushableWriter{header: http.Header{}}
	if _, err := NewSSEStream(w); err == nil {
		t.Fatal("NewSSEStream(unflushable) = nil error, want a failure")
	}
	if w.wroteHeader {
		t.Error("NewSSEStream committed the response before discovering it could not flush")
	}
}

type unflushableWriter struct {
	header      http.Header
	wroteHeader bool
}

func (u *unflushableWriter) Header() http.Header         { return u.header }
func (u *unflushableWriter) Write(b []byte) (int, error) { return len(b), nil }
func (u *unflushableWriter) WriteHeader(int)             { u.wroteHeader = true }
