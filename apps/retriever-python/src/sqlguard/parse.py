"""Turn one candidate SQL string into a parse_summary.v1 payload.

Facts only. Every function here answers "what is in this statement?" and none
answers "should it run?" — that question belongs to the Go guard, which reads
what this produces. The split is deliberate: sqlglot is the better parser and
lives in Python, while the policy that decides a rejection is the runtime's.

Nothing here raises on bad input. An unparseable generation is a routine
outcome of asking a language model for SQL, so it comes back as a summary with
``ok`` false and a classified reason, and the guard rejects it deliberately.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Final

import sqlglot
from sqlglot import exp

# Mirrors parse_summary.v1 and sql_plan.v1. A dialect outside this set is a
# caller error, not something to guess at: a summary parsed as the wrong
# dialect proves nothing about the right one.
DIALECTS: Final[frozenset[str]] = frozenset({"sqlite", "postgres"})

# Mirrors sql_plan.v1's limit_value bounds. A cap outside them would produce a
# normalized statement that fails its own contract at the guard.
MIN_LIMIT_CAP: Final[int] = 1
MAX_LIMIT_CAP: Final[int] = 10000

# Mirrors parse_summary.v1 / sql_plan.v1 maxLength for sql, and bounds the work
# the parser is asked to do on one request.
MAX_SQL_CHARS: Final[int] = 20000

# Mirrors the maxItems bounds in parse_summary.v1. Anything beyond these is
# reported truthfully as a rejectable fact rather than silently trimmed — a
# truncated table list would let the guard's subset check pass on a statement
# touching a table it never saw.
MAX_NODE_KINDS: Final[int] = 256
MAX_TABLES: Final[int] = 64
MAX_FUNCTIONS: Final[int] = 128

SCHEMA_NAME: Final[str] = "parse_summary.v1"


@dataclass(frozen=True)
class ParseRequest:
    """A validated request to summarize one statement."""

    sql: str
    dialect: str
    limit_cap: int


class ParseRequestError(ValueError):
    """The request itself was unusable, as opposed to the SQL being unparseable."""


def validate_request(payload: object) -> ParseRequest:
    """Validate an inbound request body.

    Raises:
        ParseRequestError: with a message safe to log, never echoing the SQL.
    """
    if not isinstance(payload, dict):
        raise ParseRequestError("body must be a JSON object")

    sql = payload.get("sql")
    if not isinstance(sql, str) or not sql.strip():
        raise ParseRequestError("sql: must be a non-empty string")
    if len(sql) > MAX_SQL_CHARS:
        raise ParseRequestError(f"sql: exceeds {MAX_SQL_CHARS} characters")

    dialect = payload.get("dialect")
    if dialect not in DIALECTS:
        raise ParseRequestError(f"dialect: must be one of {sorted(DIALECTS)}")

    limit_cap = payload.get("limit_cap")
    # bool is an int in Python, and True would silently become a cap of 1.
    if not isinstance(limit_cap, int) or isinstance(limit_cap, bool):
        raise ParseRequestError("limit_cap: must be an integer")
    if not MIN_LIMIT_CAP <= limit_cap <= MAX_LIMIT_CAP:
        raise ParseRequestError(f"limit_cap: must be in [{MIN_LIMIT_CAP},{MAX_LIMIT_CAP}]")

    unknown = set(payload) - {"sql", "dialect", "limit_cap"}
    if unknown:
        raise ParseRequestError(f"unknown field(s): {sorted(unknown)}")

    return ParseRequest(sql=sql, dialect=dialect, limit_cap=limit_cap)


def summarize(req: ParseRequest) -> dict[str, Any]:
    """Parse one request and return a parse_summary.v1 document.

    Never raises. The module docstring promises that, and the promise has to
    cover the WHOLE summarisation rather than only the parse call: walking a
    deeply nested statement raises RecursionError from ``walk()``, long after
    ``sqlglot.parse`` has returned successfully. A model emitting 100 nested
    subqueries is a routine bad generation, and letting it escape turned into
    an HTTP 500 that the Go client then classified as "the runtime called this
    service wrongly" — misreporting our own crash as a caller error.
    """
    try:
        return _summarize(req)
    except RecursionError:
        # Not _safe_reason: a RecursionError's message says nothing useful and
        # the type name alone is clearer.
        return _failed(req.dialect, "statement is nested too deeply to analyse")
    except Exception as exc:
        return _failed(req.dialect, _safe_reason(exc))


def _summarize(req: ParseRequest) -> dict[str, Any]:
    """The body of summarize, with failures left to the caller to classify."""
    parsed = sqlglot.parse(req.sql, read=req.dialect)

    # sqlglot yields None for an empty statement, which a trailing semicolon
    # produces. Counting those would make "SELECT 1;" look like two statements
    # and get every well-formed generation rejected as a stacked query.
    statements = [s for s in parsed if s is not None]
    if not statements:
        return _failed(req.dialect, "input contained no statement")

    node_kinds: set[str] = set()
    tables: set[str] = set()
    functions: set[str] = set()
    for statement in statements:
        node_kinds |= _node_kinds(statement)
        tables |= _tables(statement)
        functions |= _functions(statement)

    summary: dict[str, Any] = {
        "schema": SCHEMA_NAME,
        "ok": True,
        "dialect": req.dialect,
        "statement_count": len(statements),
        "node_kinds": sorted(node_kinds),
        "tables": sorted(tables),
        "functions": sorted(functions),
    }

    # A statement list this wide cannot be described within the contract's
    # bounds, and a trimmed list is worse than none: the guard's subset check
    # would pass on tables it never saw. Report the failure instead.
    if (
        len(summary["node_kinds"]) > MAX_NODE_KINDS
        or len(summary["tables"]) > MAX_TABLES
        or len(summary["functions"]) > MAX_FUNCTIONS
    ):
        return _failed(req.dialect, "statement is too complex to summarize within contract bounds")

    # Only a single statement is worth rewriting. Anything else is a stacked
    # query the guard will reject, and rendering it would hand back something
    # that looks executable.
    if len(statements) == 1:
        summary |= _limits(statements[0], req)

    return summary


def _node_kinds(statement: exp.Expr) -> set[str]:
    """Every distinct AST node type in the statement, including the root."""
    return {type(node).__name__ for node in statement.walk()}


def _tables(statement: exp.Expr) -> set[str]:
    """Real tables referenced, with CTE aliases removed.

    A common-table expression puts its own name in the AST as a table, so
    ``WITH t AS (...) SELECT * FROM t`` reports ``t`` alongside the real
    tables. Leaving it in would make the guard reject a legitimate query for
    touching a table that is not in the schema.
    """
    cte_aliases = {cte.alias_or_name.lower() for cte in statement.find_all(exp.CTE)}
    named = {table.name.lower() for table in statement.find_all(exp.Table) if table.name}
    return named - cte_aliases


def _functions(statement: exp.Expr) -> set[str]:
    """Every function called, lowercased.

    A function sqlglot knows becomes its own node type (COUNT is a Count node),
    but every function it does not know becomes one shared Anonymous node — so
    pg_read_file, readfile and lo_import are all indistinguishable by node type
    alone, and the actual name lives on the node instead.
    """
    names: set[str] = set()
    for func in statement.find_all(exp.Func):
        raw = func.this if isinstance(func, exp.Anonymous) else func.sql_name()
        name = str(raw).strip().lower()
        if name:
            names.add(name)
    return names


def _limits(statement: exp.Expr, req: ParseRequest) -> dict[str, Any]:
    """Report the statement's LIMIT and render it under the caller's cap.

    The rewrite is mechanical, not a judgement: a LIMIT already at or below the
    cap is left exactly as it was. sqlglot's ``.limit()`` overwrites
    unconditionally, so calling it on every statement would silently *raise* a
    generation's own LIMIT 10 to the cap and return more rows than the query
    asked for.
    """
    current = _limit_value(statement)
    out: dict[str, Any] = {"has_limit": current is not None}
    if current is not None:
        out["limit_value"] = current

    # Only a query has a row limit at all. A DDL root has no .limit(), and the
    # guard rejects it on node kinds regardless.
    if not isinstance(statement, exp.Query):
        return out

    if current is None:
        rendered = statement.copy().limit(req.limit_cap)
        out |= {"limit_injected": True, "limit_clamped": False}
    elif current > req.limit_cap:
        rendered = statement.copy().limit(req.limit_cap)
        out |= {"limit_injected": False, "limit_clamped": True}
    else:
        rendered = statement
        out |= {"limit_injected": False, "limit_clamped": False}

    normalized = rendered.sql(dialect=req.dialect)
    # An empty or over-long rendering cannot be carried by the contract, and
    # handing back a truncated statement would be handing back a different one.
    if not normalized or len(normalized) > MAX_SQL_CHARS:
        return out
    out["normalized_sql"] = normalized
    return out


def _limit_value(statement: exp.Expr) -> int | None:
    """The statement's own LIMIT, when it is a plain non-negative integer.

    A LIMIT computed from an expression or a parameter is deliberately reported
    as absent: it is not a bound anything can verify before execution, so the
    guard must treat it as no bound at all rather than trust it.
    """
    # sqlglot splits its hierarchy: Expr carries traversal, Expression carries
    # args, Query carries limit(). Only a real statement is all three, and
    # anything that is not has no row limit to report.
    if not isinstance(statement, exp.Expression):
        return None
    limit = statement.args.get("limit")
    if limit is None:
        return None
    value = getattr(limit, "expression", None)
    if not isinstance(value, exp.Literal) or not value.is_int:
        return None
    try:
        parsed = int(value.name)
    except ValueError:
        return None
    return parsed if parsed >= 0 else None


def _failed(dialect: str, reason: str) -> dict[str, Any]:
    """A summary for input that could not be parsed."""
    return {
        "schema": SCHEMA_NAME,
        "ok": False,
        "dialect": dialect,
        "statement_count": 0,
        "node_kinds": [],
        "tables": [],
        "functions": [],
        "error": reason,
    }


def _safe_reason(exc: Exception) -> str:
    """Classify a parser failure without quoting the input.

    sqlglot's messages embed the offending SQL and a caret diagram of it. That
    text is model output on its way to a browser, so it is replaced with a
    fixed reason here and the detail is left for the caller to log.
    """
    return f"input is not a valid statement ({type(exc).__name__})"
