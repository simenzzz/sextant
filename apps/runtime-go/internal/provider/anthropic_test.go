package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// discardLogger keeps the upstream detail out of the test output while still
// exercising the path that logs it.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sseServer serves a canned Anthropic event stream. httptest rather than a
// live endpoint: no network, no credential, no paid call, and every failure
// mode is reproducible.
func sseServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestProvider(t *testing.T, srv *httptest.Server) *AnthropicProvider {
	t.Helper()
	p, err := NewAnthropicProvider(AnthropicConfig{
		APIKey:  "test-key-not-real",
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewAnthropicProvider() error = %v", err)
	}
	return p
}

// collect drains a stream into its three observable parts.
func collect(t *testing.T, ch <-chan StreamEvent) (text string, usage *Usage, err error) {
	t.Helper()
	var b strings.Builder
	for ev := range ch {
		switch {
		case ev.Err != nil:
			err = ev.Err
		case ev.Usage != nil:
			usage = ev.Usage
		default:
			b.WriteString(ev.Delta)
		}
	}
	return b.String(), usage, err
}

const happyStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":42,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"SELECT COUNT(*) "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"FROM orders"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":17}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamAssemblesDeltasAndUsage(t *testing.T) {
	p := newTestProvider(t, sseServer(t, http.StatusOK, happyStream))

	ch, err := p.Stream(context.Background(), Request{
		Model: "claude-haiku-4-5", MaxTokens: 512,
		Messages: []Message{{Role: RoleUser, Content: "how many orders?"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	text, usage, streamErr := collect(t, ch)
	if streamErr != nil {
		t.Fatalf("stream reported %v, want no error", streamErr)
	}
	if want := "SELECT COUNT(*) FROM orders"; text != want {
		t.Errorf("assembled text = %q, want %q", text, want)
	}
	// No single event carries both counts — input arrives on message_start and
	// output on message_delta — so this is the assertion that the accumulation
	// is what makes the ledger's provider-reported numbers real.
	if usage == nil {
		t.Fatal("no Usage event; the ledger would record this paid step as unknown")
	}
	if usage.TokensIn != 42 || usage.TokensOut != 17 {
		t.Errorf("Usage = %+v, want {TokensIn:42 TokensOut:17}", *usage)
	}
}

func TestStreamReportsNoUsageWhenTheProviderReportedNone(t *testing.T) {
	// A stream with no usage at all. The adapter must stay silent rather than
	// emit a zero Usage — the ledger has to tell "cost nothing" from "nobody
	// told us", and a zeroed Usage would erase that distinction.
	const noUsage = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}

event: message_stop
data: {"type":"message_stop"}

`
	p := newTestProvider(t, sseServer(t, http.StatusOK, noUsage))

	ch, err := p.Stream(context.Background(), Request{
		Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if _, usage, _ := collect(t, ch); usage != nil {
		t.Errorf("Usage = %+v, want nil so the ledger records the cost as unknown", *usage)
	}
}

func TestStreamNeverLeaksTheUpstreamErrorBody(t *testing.T) {
	// Every one of these bodies is the kind a real provider returns, and every
	// one says something about the credential or the account that must not
	// reach a browser.
	tests := []struct {
		name       string
		status     int
		body       string
		secret     string
		wantErrHas string
	}{
		{
			name:   "401 naming the key",
			status: http.StatusUnauthorized,
			body: `{"type":"error","error":{"type":"authentication_error",` +
				`"message":"invalid x-api-key: sk-ant-SECRETKEY123"}}`,
			secret:     "sk-ant-SECRETKEY123",
			wantErrHas: "credentials",
		},
		{
			name:   "403 naming the organization",
			status: http.StatusForbidden,
			body: `{"type":"error","error":{"type":"permission_error",` +
				`"message":"org org-SECRETORG99 lacks access"}}`,
			secret:     "org-SECRETORG99",
			wantErrHas: "credentials",
		},
		{
			name:   "429 naming the quota",
			status: http.StatusTooManyRequests,
			body: `{"type":"error","error":{"type":"rate_limit_error",` +
				`"message":"limit 40000 tok/min on plan SECRETPLAN"}}`,
			secret:     "SECRETPLAN",
			wantErrHas: "rate limited",
		},
		{
			name:   "500 naming internal state",
			status: http.StatusInternalServerError,
			body: `{"type":"error","error":{"type":"api_error",` +
				`"message":"upstream shard SECRETSHARD7 unavailable"}}`,
			secret:     "SECRETSHARD7",
			wantErrHas: "unavailable",
		},
		{
			name:   "400 naming the request",
			status: http.StatusBadRequest,
			body: `{"type":"error","error":{"type":"invalid_request_error",` +
				`"message":"max_tokens SECRETDETAIL too large"}}`,
			secret:     "SECRETDETAIL",
			wantErrHas: "rejected the request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t, sseServer(t, tt.status, tt.body))

			ch, err := p.Stream(context.Background(), Request{
				Model: "claude-haiku-4-5", MaxTokens: 16,
				Messages: []Message{{Role: RoleUser, Content: "hi"}},
			})
			// The failure may surface either way round; both paths must be clean.
			var got error
			if err != nil {
				got = err
			} else {
				_, _, got = collect(t, ch)
			}
			if got == nil {
				t.Fatalf("status %d produced no error", tt.status)
			}
			if strings.Contains(got.Error(), tt.secret) {
				t.Errorf("error leaked upstream detail %q: %v", tt.secret, got)
			}
			if !strings.Contains(got.Error(), tt.wantErrHas) {
				t.Errorf("error = %q, want it to mention %q", got.Error(), tt.wantErrHas)
			}
		})
	}
}

func TestStreamStopsOnCancelledContext(t *testing.T) {
	p := newTestProvider(t, sseServer(t, http.StatusOK, happyStream))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := p.Stream(ctx, Request{
		Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		// A pre-cancelled context may fail the request outright, which is fine.
		return
	}
	// The channel must still close. A producer that ignored ctx.Done() would
	// block forever on an unread send and leak its goroutine — the failure the
	// ctx check in every send exists to prevent.
	for range ch {
	}
}

func TestNewAnthropicProviderRejectsAnUnusableConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  AnthropicConfig
	}{
		{name: "no api key", cfg: AnthropicConfig{Timeout: time.Second}},
		// The one defect PLAN.md section 12 singles out. Defaulting the
		// timeout would hide the omission rather than surface it.
		{name: "no timeout", cfg: AnthropicConfig{APIKey: "k"}},
		{name: "negative timeout", cfg: AnthropicConfig{APIKey: "k", Timeout: -time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewAnthropicProvider(tt.cfg); err == nil {
				t.Fatal("NewAnthropicProvider() = nil error, want a refusal")
			}
		})
	}
}

func TestBuildParamsRejectsUnusableRequests(t *testing.T) {
	base := Request{
		Model: "claude-haiku-4-5", MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}
	mutate := func(f func(*Request)) Request {
		r := base
		r.Messages = append([]Message(nil), base.Messages...)
		f(&r)
		return r
	}

	tests := []struct {
		name string
		req  Request
	}{
		{name: "no model", req: mutate(func(r *Request) { r.Model = "" })},
		{name: "zero max tokens", req: mutate(func(r *Request) { r.MaxTokens = 0 })},
		{name: "negative max tokens", req: mutate(func(r *Request) { r.MaxTokens = -1 })},
		{name: "no messages", req: mutate(func(r *Request) { r.Messages = nil })},
		{name: "unknown role", req: mutate(func(r *Request) { r.Messages[0].Role = "oracle" })},
		{
			// The Messages API carries the system prompt in its own field.
			// Folding a system turn into the user message would change what
			// the model is told with nobody seeing it happen.
			name: "system message in the turn list",
			req:  mutate(func(r *Request) { r.Messages[0].Role = RoleSystem }),
		},
		{
			// Claude Sonnet 5 rejects a non-default temperature with a 400.
			// PLAN.md section 5.4 samples k candidates above zero, so this
			// collides with the router at P6 — better a refusal here than
			// either a 400 on every escalated call or a silent drop that makes
			// the router think it drew k independent samples.
			name: "temperature on a model that rejects it",
			req: mutate(func(r *Request) {
				r.Model = "claude-sonnet-5"
				r.Temperature = 0.7
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildParams(tt.req); err == nil {
				t.Fatalf("buildParams(%+v) = nil error, want a refusal", tt.req)
			}
		})
	}
}

func TestBuildParamsAcceptsTemperatureOnAModelThatTakesIt(t *testing.T) {
	params, err := buildParams(Request{
		Model: "claude-haiku-4-5", MaxTokens: 16, Temperature: 0.7,
		SystemPrompt: "you write SQL",
		Messages:     []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("buildParams() error = %v", err)
	}
	if !params.Temperature.Valid() || params.Temperature.Value != 0.7 {
		t.Errorf("Temperature = %+v, want 0.7 set", params.Temperature)
	}
	if len(params.System) != 1 || params.System[0].Text != "you write SQL" {
		t.Errorf("System = %+v, want the system prompt in its own field", params.System)
	}
}

func TestClassifyKeepsCancellationDistinguishable(t *testing.T) {
	// A cancelled run is a recorded outcome, not a provider failure. Flattening
	// it into a generic message would make the agent loop unable to tell "we
	// stopped this" from "they broke".
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := classify(err); !errors.Is(got, err) {
			t.Errorf("classify(%v) = %v, want the cancellation preserved", err, got)
		}
	}
}

func TestStreamReportsTheThreeInputClassesSeparately(t *testing.T) {
	// The three bill at three different rates, so summing them would overstate
	// reads tenfold and understate writes — and leave the ledger's dollar
	// figure uncheckable by anyone reading it.
	const cachedStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":4000,"cache_creation_input_tokens":900}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`
	p := newTestProvider(t, sseServer(t, http.StatusOK, cachedStream))
	ch, err := p.Stream(context.Background(), Request{
		Model: "claude-haiku-4-5", MaxTokens: 64,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	_, usage, _ := collect(t, ch)
	if usage == nil {
		t.Fatal("no Usage event")
	}
	if usage.TokensIn != 120 {
		t.Errorf("TokensIn = %d, want 120 (standard-rate input only)", usage.TokensIn)
	}
	if usage.CacheReadTokens != 4000 {
		t.Errorf("CacheReadTokens = %d, want 4000", usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != 900 {
		t.Errorf("CacheWriteTokens = %d, want 900", usage.CacheWriteTokens)
	}
	if got := usage.TotalIn(); got != 5020 {
		t.Errorf("TotalIn() = %d, want 5020", got)
	}
}

func TestBuildParamsMarksTheSystemPromptCacheable(t *testing.T) {
	// The system prompt carries the schema card and is byte-identical for every
	// question against one database — exactly the prefix a prompt cache is for.
	//
	// Note this is INERT on a small schema: Haiku 4.5's minimum cacheable
	// prefix is 4096 tokens and the toy fixture's card is around 450, so
	// nothing is cached and the provider says so with a zero rather than an
	// error. The marker costs nothing meanwhile and the ledger already prices
	// cache tokens correctly, so the numbers are right the day it does apply.
	params, err := buildParams(Request{
		Model: "claude-haiku-4-5", MaxTokens: 64,
		SystemPrompt: "you write SQL",
		Messages:     []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("buildParams() error = %v", err)
	}
	if len(params.System) != 1 {
		t.Fatalf("System = %+v, want one block", params.System)
	}
	if params.System[0].CacheControl.Type == "" {
		t.Error("the system block carries no cache_control; the schema card would be " +
			"re-billed at the standard rate on every question once it is large enough to cache")
	}
}

func TestUsageIsNilOnlyWhenNothingWasReported(t *testing.T) {
	// A cache-only step still spent money. Treating it as "nobody told us"
	// would record a real charge as unknown.
	if usageOf(anthropicMessageWith(0, 0, 0, 0)) != nil {
		t.Error("usageOf() returned a Usage when the provider reported nothing")
	}
	if usageOf(anthropicMessageWith(0, 0, 4000, 0)) == nil {
		t.Error("usageOf() returned nil for a step that read 4000 cached tokens")
	}
}

// anthropicMessageWith builds a Message carrying just the usage counts.
func anthropicMessageWith(in, out, cacheRead, cacheWrite int64) anthropic.Message {
	var m anthropic.Message
	m.Usage.InputTokens = in
	m.Usage.OutputTokens = out
	m.Usage.CacheReadInputTokens = cacheRead
	m.Usage.CacheCreationInputTokens = cacheWrite
	return m
}
