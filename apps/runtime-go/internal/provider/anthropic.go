package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// AnthropicProvider streams completions from the Anthropic Messages API.
//
// Two things about this file are load-bearing rather than incidental.
//
// The HTTP client carries an explicit timeout. PLAN.md section 12 names
// council's missing one as the single known defect not to repeat: its provider
// built &http.Client{} bare, so a hung upstream connection was bounded only by
// the session context and a stuck request could outlive everything meant to
// bound it. NewAnthropicProvider refuses to construct without a positive
// timeout rather than defaulting to one, so the bound is a decision somebody
// made and not one this file made quietly.
//
// No upstream error body ever reaches the caller. Provider errors carry auth
// state, quota state, and organization identifiers; a 401 body in particular
// says a great deal about a credential. Every error out of here is classified
// into a fixed message, and the detail is logged server-side. This exact class
// of leak was caught by review in council.
type AnthropicProvider struct {
	client anthropic.Client
	logger *slog.Logger
}

var _ Provider = (*AnthropicProvider)(nil)

// AnthropicConfig is what the adapter needs to talk to the API.
type AnthropicConfig struct {
	// APIKey authenticates every request. Required.
	APIKey string
	// BaseURL is the API root. Config validates that it is https, or
	// loopback for the P2 replay harness.
	BaseURL string
	// Timeout bounds a single request end to end. Required and positive.
	Timeout time.Duration
	// Logger receives the upstream detail that never goes to the caller.
	Logger *slog.Logger
	// HTTPClient overrides the transport. The seam exists for P2's
	// record/replay harness, which swaps in a client reading fixtures from
	// disk so the eval subset can gate CI with zero network calls.
	HTTPClient option.HTTPClient
}

// NewAnthropicProvider builds an adapter, or fails.
func NewAnthropicProvider(cfg AnthropicConfig) (*AnthropicProvider, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("provider: anthropic requires an API key")
	}
	if cfg.Timeout <= 0 {
		// Not defaulted on purpose. A caller that forgot the timeout has
		// forgotten the one defect PLAN.md section 12 singles out, and a
		// quiet default would hide that rather than surface it.
		return nil, errors.New("provider: anthropic requires a positive HTTP timeout")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithRequestTimeout(cfg.Timeout),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	} else {
		// Belt and braces alongside WithRequestTimeout: the SDK's per-request
		// deadline governs its own retry loop, while this bounds the transport
		// itself. Neither alone covers a connection that hangs before the SDK
		// starts counting.
		opts = append(opts, option.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}))
	}

	return &AnthropicProvider{client: anthropic.NewClient(opts...), logger: logger}, nil
}

// Stream implements Provider.
//
// Contract, matching FakeProvider exactly so a test passing against the fake
// is testing the same shape the real one has: a nil channel with a non-nil
// error when the request never started, a channel closed exactly once
// otherwise, ctx.Done() checked before every send, and a Usage event before
// close whenever the provider reported one.
func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	stream := p.client.Messages.NewStreaming(ctx, params)
	// NewStreaming does not dial before the first Next, so a request that
	// never starts surfaces on the first receive rather than here. The channel
	// carries it as a terminal event, which is the in-band half of the
	// contract in provider.go.

	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		// Verified in the SDK source: Stream.Close() closes the decoder, which
		// closes the response body. Next() returning false on EOF does NOT —
		// so without this the body leaks on the SUCCESS path too, not only on
		// the early returns. Under the wall-clock cap the cancelled-mid-stream
		// path is the common one, and a leaked connection per question is a
		// file-descriptor exhaustion with a slow fuse.
		defer func() {
			if err := stream.Close(); err != nil {
				p.logger.Debug("provider: closing anthropic stream", "error", err)
			}
		}()
		p.drain(ctx, stream, out)
	}()
	return out, nil
}

// drain forwards one stream onto the channel.
//
// Accumulating into a message rather than reading usage off the raw events:
// the API reports input tokens on message_start and output tokens on
// message_delta, so no single event carries the pair. Accumulate assembles
// them, and reading the total once at the end is what keeps the ledger's
// "provider-reported, never estimated" promise honest.
func (p *AnthropicProvider) drain(
	ctx context.Context,
	stream *ssestream.Stream[anthropic.MessageStreamEventUnion],
	out chan<- StreamEvent,
) {
	var message anthropic.Message

	for stream.Next() {
		event := stream.Current()
		if err := message.Accumulate(event); err != nil {
			p.logger.Error("provider: accumulating anthropic stream", "error", err)
			p.send(ctx, out, StreamEvent{Err: errors.New("provider: malformed response stream")})
			return
		}

		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
				if !p.send(ctx, out, StreamEvent{Delta: text.Text}) {
					return
				}
			}
		}
	}

	// Usage before the error, and before close. A stream that failed partway
	// still spent the tokens it spent, and dropping them would understate the
	// run's cost — a capped run whose cost was undercounted is a cap that did
	// not do its job.
	if u := usageOf(message); u != nil {
		if !p.send(ctx, out, StreamEvent{Usage: u}) {
			return
		}
	}

	if err := stream.Err(); err != nil {
		// The only place the upstream detail exists. It carries auth and quota
		// state, so it is logged here and a classified message goes on the wire.
		p.logger.Error("provider: anthropic stream failed", "error", err)
		p.send(ctx, out, StreamEvent{Err: classify(err)})
	}
}

// send delivers one event unless the context is already done.
//
// Reports whether the send happened, so the caller stops rather than
// continuing to produce into a channel nobody will read — that is the leak the
// ctx.Done() check exists to prevent.
func (p *AnthropicProvider) send(ctx context.Context, out chan<- StreamEvent, ev StreamEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- ev:
		return true
	}
}

// usageOf extracts provider-reported token counts, or nil when none were
// reported.
//
// nil rather than a zero Usage: the ledger has to tell "this step cost
// nothing" from "nobody told us what this step cost", and cost_ledger.v1's
// cost_known flag is how it says so.
func usageOf(m anthropic.Message) *Usage {
	in := m.Usage.InputTokens + m.Usage.CacheReadInputTokens + m.Usage.CacheCreationInputTokens
	out := m.Usage.OutputTokens
	if in == 0 && out == 0 {
		return nil
	}
	return &Usage{TokensIn: int(in), TokensOut: int(out)}
}

// buildParams translates a Request into SDK parameters.
func buildParams(req Request) (anthropic.MessageNewParams, error) {
	if req.Model == "" {
		return anthropic.MessageNewParams{}, errors.New("provider: request has no model")
	}
	if req.MaxTokens <= 0 {
		return anthropic.MessageNewParams{}, fmt.Errorf(
			"provider: request has a non-positive MaxTokens (%d)", req.MaxTokens)
	}
	if len(req.Messages) == 0 {
		return anthropic.MessageNewParams{}, errors.New("provider: request has no messages")
	}

	messages := make([]anthropic.MessageParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case RoleAssistant:
			messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		case RoleSystem:
			// The Messages API carries the system prompt in its own field, not
			// as a turn. Silently folding a system message into the user turn
			// would change what the model is told without anyone seeing it.
			return anthropic.MessageNewParams{}, errors.New(
				"provider: a system message belongs in Request.SystemPrompt, not Messages")
		default:
			return anthropic.MessageNewParams{}, fmt.Errorf("provider: unknown role %q", m.Role)
		}
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  messages,
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.SystemPrompt}}
	}
	// Temperature is sent only when asked for, and only to a model that takes
	// it. Claude Sonnet 5 and the Opus 4.7+ family REJECT a non-default
	// temperature with a 400 — which collides with the k-sample
	// self-consistency check in PLAN.md section 5.4, since that samples above
	// zero. Sending it unconditionally would make every escalated call fail
	// at P6; omitting it silently would make the router think it sampled k
	// independent candidates when it drew the same one k times. Refusing is
	// the only option that is not a lie.
	if req.Temperature != 0 {
		if !acceptsTemperature(req.Model) {
			return anthropic.MessageNewParams{}, fmt.Errorf(
				"provider: model %q rejects a non-default temperature; "+
					"PLAN.md section 5.4's k-sample self-consistency needs another "+
					"source of variation on this tier", req.Model)
		}
		params.Temperature = anthropic.Float(req.Temperature)
	}
	return params, nil
}

// modelsWithoutTemperature are the models that 400 on a non-default
// temperature, top_p, or top_k.
//
// An allowlist would be the safer shape, but it would reject every model
// released after this line was written — and the failure mode there is
// refusing to run rather than a bad sample, on a knob only P6 uses. Revisit
// when the router lands.
var modelsWithoutTemperature = map[string]bool{
	"claude-sonnet-5": true,
	"claude-opus-5":   true,
	"claude-opus-4-8": true,
	"claude-opus-4-7": true,
	"claude-fable-5":  true,
}

func acceptsTemperature(model string) bool {
	return !modelsWithoutTemperature[model]
}

// classify turns an upstream failure into an error safe to put on the wire.
//
// The status code is a fact about our own request, not about the credential,
// so it is kept; the body is not. A caller needs to tell "retry might help"
// from "it will not", and nothing more.
func classify(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errors.New("provider: could not reach the model provider")
	}

	switch {
	case apiErr.StatusCode == http.StatusTooManyRequests:
		return errors.New("provider: rate limited by the model provider")
	case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden:
		// Deliberately vague to the caller. Which of the two it was, and what
		// the body said about the key, is in the server log.
		return errors.New("provider: the model provider rejected our credentials")
	case apiErr.StatusCode >= 500:
		return errors.New("provider: the model provider is unavailable")
	case apiErr.StatusCode >= 400:
		return fmt.Errorf("provider: the model provider rejected the request (status %d)",
			apiErr.StatusCode)
	default:
		return errors.New("provider: the model provider returned an unusable response")
	}
}
