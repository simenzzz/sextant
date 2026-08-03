package agent

import (
	"fmt"
	"strings"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/schema"
)

// MaxOutputTokens bounds one generation.
//
// SQL is short. A cap this size is generous for any statement a question needs
// and still bounds what one runaway generation can cost — which matters
// because the dollar cap is checked after a step completes, not during it.
const MaxOutputTokens = 1024

// systemPrompt tells the model what it is doing and what shape to answer in.
//
// Deliberately spare. The instructions that matter are the ones the guard will
// enforce anyway (one statement, SELECT only) plus the ones it cannot (use the
// sampled values). Everything else the model already knows, and a longer
// prompt is paid for on every question forever.
//
// The "sampled values" instruction is the one earning its place: without it a
// model writes `status = 'cancelled'` against a column that stores 'C', which
// is syntactically perfect, passes the guard, executes cleanly, and returns
// zero rows. That failure is invisible to every check except the answer.
const systemPrompt = `You write SQL for a read-only analytics database.

Rules:
- Answer with exactly one SELECT statement and nothing else.
- Never write DDL or DML. No INSERT, UPDATE, DELETE, DROP, ALTER, ATTACH, or PRAGMA.
- Use only the tables and columns in the schema below. Do not invent names.
- Read the sampled values. A column often stores a code rather than a word, and
  a filter written against the word you expected will return no rows.
- Prefer explicit JOINs over correlated subqueries where both work.
- Put the statement in a ` + "```sql" + ` block.`

// BuildRequest assembles the generation request for one question.
//
// The ordering is not arbitrary. The system prompt and the schema card are
// identical for every question against the same database, and the question is
// not — so the stable part goes first and the volatile part last. That is the
// prefix the provider's prompt cache reuses (the adapter marks it), and
// getting the order wrong would make every question a cache miss.
//
// It is worth knowing that this is INERT on a small schema, and silently so.
// Each model has a minimum cacheable prefix and Haiku 4.5's is 4096 tokens —
// the highest of any current model, and not monotonic across generations
// (Opus 5's is 512). The toy fixture's system prompt measures about 450
// tokens, so nothing caches and the provider reports a zero rather than an
// error. It begins paying off when the schema grows: PLAN.md section 11.1's
// corpus at P5, or BIRD's larger databases.
func BuildRequest(model string, sch schema.Schema, question, evidence string) provider.Request {
	var system strings.Builder
	system.WriteString(systemPrompt)
	system.WriteString("\n\nSchema:\n\n")
	system.WriteString(sch.Card())

	var user strings.Builder
	if evidence = strings.TrimSpace(evidence); evidence != "" {
		// BIRD supplies this per item and it is often the only thing that
		// makes a question answerable — a domain definition, a unit, a
		// formula. It goes before the question because it is context for it.
		fmt.Fprintf(&user, "Context: %s\n\n", evidence)
	}
	fmt.Fprintf(&user, "Question: %s", strings.TrimSpace(question))

	return provider.Request{
		SystemPrompt: system.String(),
		Messages:     []provider.Message{{Role: provider.RoleUser, Content: user.String()}},
		Model:        model,
		MaxTokens:    MaxOutputTokens,
		// Temperature is left at zero. P1 generates one candidate, so there is
		// nothing to vary; k-sample self-consistency arrives at P6, and the
		// provider adapter refuses a non-default temperature on the tiers that
		// reject one.
	}
}
