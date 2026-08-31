"""The summary must describe the statement the guard will authorise.

Two facts that a bare-name summary got wrong, both found by security review of
P1. Neither is about the verdict — that is the Go guard's policy — but about
whether the summary carries enough for the guard to reach a correct one.
"""

from __future__ import annotations

import pytest

from src.contracts import validate_document
from src.sqlguard.parse import ParseRequest, summarize


def _summary(sql: str, dialect: str = "sqlite", limit_cap: int = 500) -> dict:
    summary = summarize(ParseRequest(sql=sql, dialect=dialect, limit_cap=limit_cap))
    validate_document("parse_summary.v1", summary)
    return summary


@pytest.mark.parametrize(
    ("sql", "dialect", "expected"),
    [
        # The qualifier stays in the rendered statement, so it has to stay in
        # the reported name. Reporting "users" for "other.users" would let a
        # guard whose allowed set contains "users" approve a read of a table
        # generation was never shown.
        ("SELECT * FROM other.users", "postgres", {"other.users"}),
        ("SELECT * FROM db.secret.users", "postgres", {"db.secret.users"}),
        (
            "SELECT * FROM other.users JOIN users ON 1=1",
            "postgres",
            {"other.users", "users"},
        ),
        ("SELECT * FROM main.orders", "sqlite", {"main.orders"}),
        ("SELECT * FROM temp.orders", "sqlite", {"temp.orders"}),
        # An unqualified reference is unchanged.
        ("SELECT * FROM orders", "sqlite", {"orders"}),
        # A CTE alias is still not a table, qualifier or no qualifier.
        (
            "WITH t AS (SELECT * FROM other.orders) SELECT * FROM t",
            "postgres",
            {"other.orders"},
        ),
    ],
)
def test_a_schema_qualifier_stays_in_the_reported_table_name(sql, dialect, expected):
    assert set(_summary(sql, dialect)["tables"]) == expected


@pytest.mark.parametrize(
    ("sql", "dialect"),
    [
        # The classic breakout: close the guard's block comment, stack a
        # statement, reopen. Only sqlglot's own sanitiser stood between this
        # and the executed text, so the comment is now removed outright.
        ("SELECT name FROM users -- */ ; DROP TABLE users; /*", "postgres"),
        ("SELECT /* ; DROP TABLE users; */ name FROM users", "sqlite"),
        ("-- ; DROP TABLE users\nSELECT name FROM users", "sqlite"),
    ],
)
def test_comments_never_reach_the_statement_that_executes(sql, dialect):
    normalized = _summary(sql, dialect).get("normalized_sql", "")

    assert normalized, "an otherwise valid statement produced no rendering"
    # Nothing downstream reads comments, so the rendered statement carries
    # none — which removes model-controlled text from the string the guard
    # authorises and the driver runs.
    for marker in ("--", "/*", "*/", "DROP", "drop"):
        assert marker not in normalized, f"{marker!r} survived into {normalized!r}"


def test_stripping_comments_does_not_change_the_query():
    summary = _summary("SELECT id /* the order id */ FROM orders LIMIT 10", "sqlite")

    assert summary["normalized_sql"] == "SELECT id FROM orders LIMIT 10"
    assert summary["tables"] == ["orders"]
    assert summary["limit_value"] == 10
    assert summary["limit_injected"] is False
    assert summary["limit_clamped"] is False


def test_a_comment_breakout_is_still_reported_as_a_stacked_query():
    """Closing the comment for real does not hide the statements behind it.

    This is the shape the comment stripping is a second line of defence for:
    here the breakout parses as three statements, so the guard refuses it on
    the statement count and on the DROP node kind, and no rendering is offered
    at all.
    """
    summary = _summary(
        "SELECT name FROM users /* */ ; DROP TABLE users; /* */", "postgres"
    )

    assert summary["statement_count"] == 3
    assert "Drop" in summary["node_kinds"]
    # Nothing to execute is offered for a statement the guard must refuse.
    assert summary.get("normalized_sql") is None
