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
		return gen.ResultSetV1{}, fmt.Errorf("executor: running statement: %w", err)
	}
	defer rows.Close()

	result, err := e.scan(rows, runID, plan)
	if err != nil {
		return gen.ResultSetV1{}, err
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

		if result.Truncated {
			continue
		}
		if size, err := marshalledSize(cells); err != nil || size > budget {
			result.Truncated = true
			continue
		} else {
			budget -= size
		}
		result.Rows = append(result.Rows, cells)
	}
	if err := rows.Err(); err != nil {
		return gen.ResultSetV1{}, fmt.Errorf("executor: reading rows: %w", err)
	}
	return result, nil
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
