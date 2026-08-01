// Package contracts validates documents against the shared JSON Schema
// contracts in packages/contracts/schemas.
//
// The schemas are embedded from gen/schemas, which codegen copies verbatim
// from the source of truth. Embedding the copy rather than reading the
// originals at runtime means the binary carries the exact schemas its
// generated types were built from, and the CI drift gate is what keeps the
// copy honest.
//
// House rule this serves: validate at every boundary. A trace event is checked
// before it goes on the wire, a question request before it is acted on.
package contracts

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed gen/schemas/*.schema.json
var schemaFS embed.FS

// Name identifies a contract by its discriminator, e.g. "trace_event.v1".
type Name string

const (
	QuestionRequestV1 Name = "question_request.v1"
	TraceEventV1      Name = "trace_event.v1"
	SQLPlanV1         Name = "sql_plan.v1"
	CostLedgerV1      Name = "cost_ledger.v1"
	EvalResultV1      Name = "eval_result.v1"
)

const schemaDir = "gen/schemas"

// compiled holds one validator per contract, built once at init. Compilation
// is not cheap and the schema set is fixed at build time, so doing it per call
// would be pure waste on the request path.
var compiled = mustCompileAll()

// ErrUnknownContract is returned when a name has no embedded schema.
type ErrUnknownContract struct{ Name Name }

func (e ErrUnknownContract) Error() string {
	return fmt.Sprintf("contracts: unknown contract %q", e.Name)
}

// ValidationError reports a document that failed its contract.
type ValidationError struct {
	Name   Name
	Detail string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("contracts: %s: %s", e.Name, e.Detail)
}

// SafeMessage is what may be shown to a client.
//
// Error() quotes the offending instance values, which for a failed LLM or
// driver response means echoing untrusted — potentially sensitive — content
// straight back to the browser. Handlers log Error() and return this.
func (e ValidationError) SafeMessage() string {
	return fmt.Sprintf("contracts: %s: document failed validation", e.Name)
}

// Names returns every contract this build knows about, for tests and
// diagnostics. Order is not guaranteed.
func Names() []Name {
	out := make([]Name, 0, len(compiled))
	for n := range compiled {
		out = append(out, n)
	}
	return out
}

// Validate checks raw JSON against the named contract.
//
// It returns ErrUnknownContract for a name with no schema and ValidationError
// for a document that does not conform. Both are ordinary errors: callers at a
// boundary should reject the input, not panic.
func Validate(name Name, raw []byte) error {
	sch, ok := compiled[name]
	if !ok {
		return ErrUnknownContract{Name: name}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return ValidationError{Name: name, Detail: fmt.Sprintf("malformed JSON: %v", err)}
	}
	if err := sch.Validate(doc); err != nil {
		return ValidationError{Name: name, Detail: err.Error()}
	}
	return nil
}

func mustCompileAll() map[Name]*jsonschema.Schema {
	out, err := compileAll()
	if err != nil {
		// A build whose embedded schemas do not compile is broken in a way no
		// caller can recover from, and failing at init surfaces it on the
		// first test rather than on the first request.
		panic(fmt.Sprintf("contracts: compiling embedded schemas: %v", err))
	}
	return out
}

func compileAll() (map[Name]*jsonschema.Schema, error) {
	entries, err := fs.ReadDir(schemaFS, schemaDir)
	if err != nil {
		return nil, fmt.Errorf("reading embedded schema dir: %w", err)
	}

	c := jsonschema.NewCompiler()
	// Formats are assertions here, not annotations. All three language suites
	// run the same fixture corpus, so they must agree on whether "last tuesday"
	// is a date-time. Leaving formats as annotations would make the Go suite
	// accept fixtures the Python and TypeScript suites reject.
	c.AssertFormat()

	names := make([]Name, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		p := path.Join(schemaDir, e.Name())
		raw, err := schemaFS.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", p, err)
		}
		if err := c.AddResource(e.Name(), doc); err != nil {
			return nil, fmt.Errorf("adding %s: %w", p, err)
		}
		names = append(names, Name(strings.TrimSuffix(e.Name(), ".schema.json")))
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("no schemas embedded from %s", schemaDir)
	}

	out := make(map[Name]*jsonschema.Schema, len(names))
	for _, n := range names {
		sch, err := c.Compile(string(n) + ".schema.json")
		if err != nil {
			return nil, fmt.Errorf("compiling %s: %w", n, err)
		}
		out[n] = sch
	}
	return out, nil
}
