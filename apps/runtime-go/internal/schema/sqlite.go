package schema

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// IntrospectSQLite reads a SQLite database's schema.
//
// Every statement here runs against a connection the caller opened read-only.
// This package never opens one itself, so there is no path by which schema
// reading could reach a writable handle.
func IntrospectSQLite(ctx context.Context, db *sql.DB) (Schema, error) {
	names, err := sqliteTableNames(ctx, db)
	if err != nil {
		return Schema{}, err
	}

	tables := make([]Table, 0, len(names))
	for _, name := range names {
		table, err := sqliteTable(ctx, db, name)
		if err != nil {
			return Schema{}, err
		}
		tables = append(tables, table)
	}
	return Schema{Tables: tables}, nil
}

// sqliteTableNames lists user tables, in a stable order.
//
// ORDER BY name rather than sqlite_master's natural order: the fingerprint and
// the prompt cache both depend on the card rendering identically every time,
// and natural order is not guaranteed stable across vacuum or a rebuild.
func sqliteTableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	const q = `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("schema: listing tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("schema: scanning table name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func sqliteTable(ctx context.Context, db *sql.DB, name string) (Table, error) {
	columns, err := sqliteColumns(ctx, db, name)
	if err != nil {
		return Table{}, err
	}
	keys, err := sqliteForeignKeys(ctx, db, name)
	if err != nil {
		return Table{}, err
	}

	for i := range columns {
		samples, err := sqliteSamples(ctx, db, name, columns[i].Name)
		if err != nil {
			return Table{}, err
		}
		columns[i].Samples = samples
	}
	return Table{Name: name, Columns: columns, ForeignKeys: keys}, nil
}

func sqliteColumns(ctx context.Context, db *sql.DB, table string) ([]Column, error) {
	// PRAGMA takes an identifier, not a bind parameter — no driver will
	// substitute a ? here. quoteIdent is what makes that safe, and the name
	// came from sqlite_master rather than from a request.
	q := fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table))

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("schema: reading columns of %s: %w", table, err)
	}
	defer rows.Close()

	var out []Column
	for rows.Next() {
		var (
			cid        int
			name       string
			declType   sql.NullString
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &defaultVal, &pk); err != nil {
			return nil, fmt.Errorf("schema: scanning column of %s: %w", table, err)
		}
		out = append(out, Column{
			Name:       name,
			Type:       strings.TrimSpace(declType.String),
			PrimaryKey: pk > 0,
			NotNull:    notNull != 0,
		})
	}
	return out, rows.Err()
}

func sqliteForeignKeys(ctx context.Context, db *sql.DB, table string) ([]ForeignKey, error) {
	q := fmt.Sprintf("PRAGMA foreign_key_list(%s)", quoteIdent(table))

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("schema: reading foreign keys of %s: %w", table, err)
	}
	defer rows.Close()

	var out []ForeignKey
	for rows.Next() {
		var (
			id, seq                     int
			refTable, from, to          sql.NullString
			onUpdate, onDelete, matchCl sql.NullString
		)
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &matchCl); err != nil {
			return nil, fmt.Errorf("schema: scanning foreign key of %s: %w", table, err)
		}
		// `to` is null when the reference targets the parent's primary key
		// implicitly. Rendering an empty column name would produce a card that
		// reads as broken, so it becomes the conventional marker instead.
		target := to.String
		if target == "" {
			target = "rowid"
		}
		out = append(out, ForeignKey{
			Column:    from.String,
			RefTable:  refTable.String,
			RefColumn: target,
		})
	}
	return out, rows.Err()
}

// sqliteSamples reads up to SampleValuesPerColumn distinct non-null values.
//
// The values are what make an encoded column legible to generation. Long ones
// are truncated: a single wide TEXT column would otherwise dominate a prompt
// that is paid for on every question.
func sqliteSamples(ctx context.Context, db *sql.DB, table, column string) ([]string, error) {
	q := fmt.Sprintf(
		"SELECT DISTINCT %s FROM %s WHERE %s IS NOT NULL ORDER BY %s LIMIT %d",
		quoteIdent(column), quoteIdent(table), quoteIdent(column), quoteIdent(column),
		SampleValuesPerColumn)

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		// Not fatal. A column can be unsamplable — a type the driver will not
		// scan, a generated column — and a schema card without one column's
		// examples is far better than no schema card at all.
		return nil, nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return out, nil
		}
		if v.Valid {
			out = append(out, truncate(v.String, maxSampleChars))
		}
	}
	if err := rows.Err(); err != nil {
		return out, nil
	}
	return out, nil
}

// maxSampleChars bounds one sampled value in the prompt.
const maxSampleChars = 48

func truncate(s string, max int) string {
	// Runes, not bytes: slicing a UTF-8 string at a byte offset can split a
	// codepoint and put a replacement character into the prompt. The corpus
	// PLAN.md section 11.1 chose stores Portuguese category names, so this is
	// a real case and not a hypothetical one.
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// quoteIdent quotes a SQL identifier for SQLite.
//
// Identifiers cannot be bind parameters, so PRAGMA and the sample query build
// them into the statement. These names come from sqlite_master — the database
// describing itself — but quoting is what keeps that a fact about today's call
// graph rather than a load-bearing assumption: a table named `order` or
// `my"table` must not change the shape of the statement around it.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
