// Package executor runs a validated plan against a read-only database.
//
// It takes a sql_plan.v1, never a raw string. That is the type-level statement
// of the invariant: there is no function in this package that will execute SQL
// the guard has not cleared, so "did this go through the guard?" is answered by
// the signature rather than by reading call sites.
package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// DefaultMaxResultBytes bounds the marshalled result_set.v1 frame.
//
// The schema bounds the result for CORRECTNESS (at most 64 columns, at most
// 10000 rows, cells at most 4096 characters); this bounds it for SIZE. The two
// are different jobs: a legal result at those maxima would be hundreds of
// megabytes, which no browser should be asked to JSON.parse.
const DefaultMaxResultBytes = 1 << 20 // 1 MiB

// MaxResultBytesCeiling is the largest budget an operator may configure.
//
// It must stay equal to MAX_RESULT_FRAME_CHARS in apps/web/src/lib/protocol.ts.
// That constant is the browser's ceiling for a result frame, so a runtime
// permitted to emit a larger one would produce frames its own client rejects —
// and the failure would look like a network fault rather than a config
// mismatch. executor_test.go asserts this value; protocol.test.ts asserts the
// other; the two are checked against the same number.
const MaxResultBytesCeiling = 4 << 20 // 4 MiB

// Executor runs plans against one database.
type Executor struct {
	db    *sql.DB
	clk   clock.Clock
	limit int
}

// New builds an Executor over an already-open, read-only handle.
//
// The handle is the caller's: this package does not decide what it is
// connected to, and cannot silently open something writable.
func New(db *sql.DB, clk clock.Clock, maxResultBytes int) (*Executor, error) {
	if db == nil {
		return nil, errors.New("executor: nil database handle")
	}
	if clk == nil {
		return nil, errors.New("executor: nil clock")
	}
	if maxResultBytes <= 0 {
		maxResultBytes = DefaultMaxResultBytes
	}
	if maxResultBytes > MaxResultBytesCeiling {
		return nil, fmt.Errorf(
			"executor: max result bytes %d exceeds the ceiling %d that the browser accepts",
			maxResultBytes, MaxResultBytesCeiling)
	}
	return &Executor{db: db, clk: clk, limit: maxResultBytes}, nil
}

// Execute runs a cleared plan and returns its rows.
//
// The plan's statement timeout is enforced with a context deadline, which is
// what actually stops a runaway query: the LIMIT the guard injected bounds how
// many rows come back, not how long the database spends deciding that. A
// recursive CTE or a cartesian product can burn minutes before producing a
// first row, and only the deadline ends it.
func (e *Executor) Execute(ctx context.Context, runID string, plan gen.SqlPlanV1) (gen.ResultSetV1, error) {
	if plan.Sql == "" {
		return gen.ResultSetV1{}, errors.New("executor: plan carries no statement")
	}
	if plan.StatementTimeoutMs <= 0 {
		// sql_plan.v1 requires a positive timeout. A plan without one would
		// run unbounded, so this is a refusal rather than a default.
		return gen.ResultSetV1{}, errors.New("executor: plan carries no statement timeout")
	}

	start := e.clk.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(plan.StatementTimeoutMs)*time.Millisecond)
	defer cancel()

	rows, err := e.db.QueryContext(ctx, plan.Sql)
	if err != nil {
		// Classified HERE, at this package's own boundary, exactly as the
		// provider and the parser client classify at theirs. Relying on the
		// agent loop to remember not to forward a driver error would be
		// relying on the one place the guidance is only a comment — and at P3
		// a Postgres connection error carries host, port, user, and sometimes
		// the database name.
		return gen.ResultSetV1{}, e.classify(err)
	}
	defer rows.Close()

	result, err := e.scan(rows, runID, plan)
	if err != nil {
		return gen.ResultSetV1{}, err
	}
	if err := rows.Err(); err != nil {
		return gen.ResultSetV1{}, e.classify(err)
	}
	elapsed := clock.ElapsedMS(e.clk, start)
	result.ElapsedMs = &elapsed
	return result, nil
}

func (e *Executor) scan(rows *sql.Rows, runID string, plan gen.SqlPlanV1) (gen.ResultSetV1, error) {
	columns, err := buildColumns(rows)
	if err != nil {
		return gen.ResultSetV1{}, err
	}

	result := gen.ResultSetV1{
		Schema:    "result_set.v1",
		RunId:     runID,
		Columns:   columns,
		Rows:      [][]gen.Cell{},
		Truncated: false,
	}
	if plan.SchemaFingerprint != "" {
		result.SchemaFingerprint = &plan.SchemaFingerprint
	}

	// budget tracks the marshalled size as rows accumulate, so the frame stops
	// growing at the budget instead of being built in full and then measured —
	// a 200 MB intermediate is still a 200 MB allocation even if it is
	// discarded afterwards.
	budget := e.limit

	for rows.Next() {
		cells, err := scanRow(rows, len(columns))
		if err != nil {
			return gen.ResultSetV1{}, err
		}
		// row_count is the honest count of what the executor read, and it
		// keeps counting past the point where rows stop being carried — a
		// truncated frame must never understate the result.
		result.RowCount++

		// An independent row ceiling. The LIMIT is the guard's, and the byte
		// budget bounds the FRAME rather than the scan — so without this a
		// narrow result can exceed result_set.v1's 10000-row maximum inside
		// 1 MiB, fail contract validation on the way out, and be dropped. The
		// user then gets no answer and only a server log says why.
		if result.RowCount > maxScannedRows {
			result.Truncated = true
			break
		}

		if result.Truncated {
			continue
		}
		size, err := marshalledSize(cells)
		if err != nil {
			// NOT the same as hitting the byte budget. Laundering an
			// unmarshallable cell into "truncated" tells the user their result
			// was too big, which is undiagnosable and untrue.
			return gen.ResultSetV1{}, fmt.Errorf("executor: encoding row %d: %w", result.RowCount, err)
		}
		if size > budget {
			result.Truncated = true
			continue
		}
		budget -= size
		result.Rows = append(result.Rows, cells)
	}
	return result, nil
}

// maxScannedRows mirrors result_set.v1's rows maxItems.
//
// Reading past it cannot produce a document the contract admits, so stopping
// there costs nothing and prevents a frame that would be dropped downstream.
const maxScannedRows = 10000

// Failure kinds this package reports, from trace_event.v1's enum.
const (
	KindTimeout       = "timeout"
	KindUnknownTable  = "unknown_table"
	KindUnknownColumn = "unknown_column"
	KindSyntaxError   = "syntax_error"
	KindTypeMismatch  = "type_mismatch"
	KindInternalError = "internal_error"
)

// Failure is a classified execution error.
//
// Kind is what the P4 repair loop keys its next prompt on — an unknown column
// and a timeout deserve completely different follow-ups. Message is safe to
// show; the driver's own text stays in the log.
type Failure struct {
	Kind    string
	Message string
}

func (f *Failure) Error() string { return fmt.Sprintf("executor: %s: %s", f.Kind, f.Message) }

// classify turns a driver error into something safe to report.
//
// A full taxonomy arrives at P3 with guard.Classify, against the adversarial
// corpus and both dialects. This is the subset P1 needs, and it exists now so
// the loop author cannot forward a raw driver error by omission.
func (e *Executor) classify(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Failure{Kind: KindTimeout, Message: "the query took too long and was stopped"}
	}
	if errors.Is(err, context.Canceled) {
		return &Failure{Kind: KindInternalError, Message: "the query was cancelled"}
	}

	// Matched on the driver's text because database/sql exposes no typed
	// errors for these. Deliberately coarse: a wrong classification only
	// changes which repair prompt P4 picks, whereas forwarding the raw message
	// would be a leak.
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "no such table"), strings.Contains(lower, "does not exist"):
		return &Failure{Kind: KindUnknownTable, Message: "the query referenced a table that does not exist"}
	case strings.Contains(lower, "no such column"), strings.Contains(lower, "no such function"):
		return &Failure{Kind: KindUnknownColumn, Message: "the query referenced a column or function that does not exist"}
	case strings.Contains(lower, "syntax error"):
		return &Failure{Kind: KindSyntaxError, Message: "the database could not parse the statement"}
	case strings.Contains(lower, "datatype mismatch"), strings.Contains(lower, "type mismatch"):
		return &Failure{Kind: KindTypeMismatch, Message: "the query compared values of incompatible types"}
	default:
		return &Failure{Kind: KindInternalError, Message: "the query could not be run"}
	}
}

func buildColumns(rows *sql.Rows) ([]gen.Column, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("executor: reading column names: %w", err)
	}
	if len(names) == 0 {
		// result_set.v1 requires at least one column, and a statement the
		// guard cleared as a SELECT always has one.
		return nil, errors.New("executor: statement returned no columns")
	}

	// Declared types are best-effort. SQLite reports nothing for a computed
	// column, and an absent type is an honest answer where a guessed one
	// would drive the wrong chart at P8.
	types, _ := rows.ColumnTypes()

	out := make([]gen.Column, len(names))
	for i, name := range names {
		out[i] = gen.Column{Name: name}
		if types != nil && i < len(types) {
			if t := types[i].DatabaseTypeName(); t != "" {
				declared := t
				out[i].Type = &declared
			}
		}
	}
	return out, nil
}

// scanRow reads one row into contract cells.
//
// Scanning into `any` and converting afterwards, rather than into typed
// destinations: the guard clears arbitrary SELECT statements, so the column
// types are not known at compile time. []byte becomes a string because the
// driver hands back TEXT that way and a raw []byte would marshal to base64 —
// silently turning a name into gibberish in the result table.
func scanRow(rows *sql.Rows, width int) ([]gen.Cell, error) {
	raw := make([]any, width)
	dest := make([]any, width)
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, fmt.Errorf("executor: scanning row: %w", err)
	}

	cells := make([]gen.Cell, width)
	for i, v := range raw {
		cells[i] = toCell(v)
	}
	return cells, nil
}

// toCell converts a driver value into something result_set.v1 admits.
//
// NULL stays nil rather than becoming "": a null is a real SQL answer, and
// coercing it would make the rendered table disagree with the query.
func toCell(v any) gen.Cell {
	switch value := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(value)
	case time.Time:
		// A driver-native time has no representation in the contract's cell
		// union. RFC 3339 is the one form every consumer can parse back.
		return value.Format(time.RFC3339Nano)
	default:
		return value
	}
}

// marshalledSize reports how many bytes a row adds to the frame.
func marshalledSize(cells []gen.Cell) (int, error) {
	encoded, err := json.Marshal(cells)
	if err != nil {
		return 0, err
	}
	// +1 for the comma separating this row from the previous one. Approximate
	// by a byte, which is the direction that overestimates.
	return len(encoded) + 1, nil
}
