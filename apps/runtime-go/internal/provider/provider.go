// Package provider is the runtime's boundary to an LLM.
//
// The whole abstraction is one method returning a channel. Errors are in-band
// (StreamEvent.Err) for failures that happen mid-stream, and out-of-band (the
// second return) for failures that mean no stream ever started — the caller
// needs to tell those apart to decide whether a partial generation is worth
// keeping.
//
// Usage is reported in-band too, on a final event. Cost accounting depends on
// provider-reported token counts rather than estimates, so a stream that ends
// without a Usage event is a stream whose cost is unknown, and the ledger says
// so instead of guessing.
package provider

import "context"

// Role identifies who authored a message in the conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of conversation.
type Message struct {
	Role    Role
	Content string
}

// Request is a single completion request.
//
// Temperature drives the k-sample self-consistency check in PLAN.md 5.4: the
// caller issues k requests above temperature zero and compares their *result
// sets*, not their SQL text. Sampling count is the caller's loop rather than a
// field here, so every candidate is an independent stream that can fail, time
// out, and be charged on its own.
type Request struct {
	SystemPrompt string
	Messages     []Message
	Model        string
	Temperature  float64
	MaxTokens    int
}

// Usage is what the provider reports it charged for. These are the numbers the
// cost ledger records; the runtime never estimates token counts.
//
// Input arrives in three classes because they are billed at three different
// rates, and summing them would make the ledger wrong rather than merely
// coarse: a cache read costs about a tenth of a standard input token and a
// cache write about a quarter more. One combined figure overstates reads and
// understates writes, and no consumer could recover the truth from it.
type Usage struct {
	// TokensIn is input processed at the standard rate — neither read from
	// nor written to the provider's prompt cache.
	TokensIn int
	// TokensOut is generated output.
	TokensOut int
	// CacheReadTokens were served from the provider's prompt cache.
	CacheReadTokens int
	// CacheWriteTokens were written into it.
	CacheWriteTokens int
}

// TotalIn is every input token, regardless of how it was billed.
func (u Usage) TotalIn() int {
	return u.TokensIn + u.CacheReadTokens + u.CacheWriteTokens
}

// StreamEvent is one item on a provider stream: a token fragment, a terminal
// error, or the final usage report. Exactly one of the three is meaningful.
type StreamEvent struct {
	Delta string
	Usage *Usage
	Err   error
}

// Provider streams a completion.
//
// Implementations must:
//   - return a nil channel and a non-nil error when the request could not be
//     started at all;
//   - close the returned channel exactly once, when the stream is over;
//   - check ctx.Done() before every send, so a cancelled run does not leak the
//     producing goroutine;
//   - emit a StreamEvent carrying Usage before closing, whenever the provider
//     reported it.
type Provider interface {
	Stream(ctx context.Context, req Request) (<-chan StreamEvent, error)
}
