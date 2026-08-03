"""Table-driven tests for the SQL parse summariser.

These assert the summary is ACCURATE, never that a statement is rejected.
Rejection is the Go guard's policy and is tested there; if these tests started
asserting "DROP TABLE is refused" the policy would exist in two places and
could disagree with itself.

The adversarial corpus from PLAN.md section 5.2 appears here anyway — not to
check the verdict, but to prove the summary carries enough for the guard to
reach one. A stacked query has to report two statements; a DDL statement has to
report its node kind; a file-reading function has to report its name.
"""

from __future__ import annotations

import pytest

from src.contracts import validate_document
from src.sqlguard.parse import (
    ParseRequest,
    ParseRequestError,
    summarize,
    validate_request,
)


def _summary(sql: str, dialect: str = "sqlite", limit_cap: int = 500) -> dict:
    summary = summarize(ParseRequest(sql=sql, dialect=dialect, limit_cap=limit_cap))
    # Every summary these tests look at is one the endpoint would emit, so it
    # must conform. This catches a shape error once instead of in each case.
    validate_document("parse_summary.v1", summary)
    return summary


class TestStatementCount:
    @pytest.mark.parametrize(
        ("sql", "want"),
        [
            ("SELECT 1", 1),
            # A trailing semicolon parses to a trailing empty statement. Counting
            # it would make every well-formed generation look like a stacked
            # query and get it rejected.
            ("SELECT 1;", 1),
            ("SELECT 1;;", 1),
            ("SELECT 1  ;  ", 1),
            ("SELECT 1; DELETE FROM users", 2),
            ("SELECT 1; SELECT 2; SELECT 3", 3),
        ],
    )
    def test_counts_real_statements_only(self, sql: str, want: int) -> None:
        assert _summary(sql)["statement_count"] == want


class TestNodeKinds:
    @pytest.mark.parametrize(
        ("sql", "expected_kind"),
        [
            ("DROP TABLE users", "Drop"),
            ("DELETE FROM users", "Delete"),
            ("INSERT INTO users (id) VALUES (1)", "Insert"),
            ("UPDATE users SET id = 2", "Update"),
            ("ATTACH DATABASE 'x.db' AS x", "Attach"),
            ("PRAGMA table_info(orders)", "Pragma"),
            ("CREATE TABLE t (id INT)", "Create"),
        ],
    )
    def test_reports_the_node_kind_the_guard_will_reject_on(
        self, sql: str, expected_kind: str
    ) -> None:
        # The guard allowlists node kinds, so what matters here is that the
        # damaging construct is NAMED. If a sqlglot upgrade renamed these, the
        # guard's allowlist would silently stop matching and this catches it.
        assert expected_kind in _summary(sql)["node_kinds"]

    def test_reports_the_root_kind_too(self) -> None:
        assert "Select" in _summary("SELECT 1")["node_kinds"]

    def test_a_plain_select_names_no_mutating_kind(self) -> None:
        kinds = set(_summary("SELECT COUNT(*) FROM orders WHERE status = 'C'")["node_kinds"])
        assert kinds.isdisjoint({"Drop", "Delete", "Insert", "Update", "Attach", "Pragma"})

    def test_a_comment_does_not_hide_the_statement(self) -> None:
        # The classic injection shape. sqlglot discards the comment, so the
        # summary describes the real statement rather than the decoration.
        summary = _summary("SELECT * FROM users -- ' OR 1=1")
        assert summary["statement_count"] == 1
        assert summary["tables"] == ["users"]


class TestTables:
    @pytest.mark.parametrize(
        ("sql", "want"),
        [
            ("SELECT * FROM orders", ["orders"]),
            ("SELECT * FROM main.orders", ["orders"]),
            ("SELECT * FROM Orders", ["orders"]),
            (
                "SELECT c.name FROM customers c JOIN orders o ON o.customer_id = c.id",
                ["customers", "orders"],
            ),
            # A CTE names itself as a table in the AST. Reporting it would make
            # the guard reject a legitimate query for touching a table that is
            # not in the schema.
            ("WITH t AS (SELECT id FROM orders) SELECT * FROM t", ["orders"]),
            (
                "WITH a AS (SELECT 1), b AS (SELECT * FROM a) "
                "SELECT * FROM b JOIN orders ON 1 = 1",
                ["orders"],
            ),
            # A query touching nothing is a real parse and an empty table list.
            # sql_plan.v1 requires at least one table, so the guard rejects it —
            # but that is the guard's call to make, on an accurate summary.
            ("SELECT 1", []),
        ],
    )
    def test_reports_real_tables_lowercased(self, sql: str, want: list[str]) -> None:
        assert _summary(sql)["tables"] == want

    def test_a_stacked_query_reports_tables_from_every_statement(self) -> None:
        assert _summary("SELECT * FROM orders; DROP TABLE users")["tables"] == [
            "orders",
            "users",
        ]


class TestFunctions:
    @pytest.mark.parametrize(
        ("sql", "dialect", "expected"),
        [
            ("SELECT COUNT(*) FROM t", "sqlite", "count"),
            ("SELECT upper(name) FROM t", "sqlite", "upper"),
            # The reason this field exists: a parser renders functions it knows
            # as their own node type, and everything else as one shared kind, so
            # node_kinds alone cannot tell pg_read_file from COUNT.
            ("SELECT pg_read_file('/etc/passwd')", "postgres", "pg_read_file"),
            ("SELECT lo_import('/etc/passwd')", "postgres", "lo_import"),
            ("SELECT readfile('x')", "sqlite", "readfile"),
            ("SELECT writefile('a', 'b')", "sqlite", "writefile"),
        ],
    )
    def test_names_the_function(self, sql: str, dialect: str, expected: str) -> None:
        assert expected in _summary(sql, dialect=dialect)["functions"]

    def test_a_query_calling_nothing_reports_no_functions(self) -> None:
        assert _summary("SELECT * FROM orders")["functions"] == []


class TestLimits:
    def test_injects_a_limit_when_the_statement_has_none(self) -> None:
        summary = _summary("SELECT * FROM orders", limit_cap=500)
        assert summary["has_limit"] is False
        assert summary["limit_injected"] is True
        assert summary["limit_clamped"] is False
        assert summary["normalized_sql"].endswith("LIMIT 500")

    def test_clamps_a_limit_above_the_cap(self) -> None:
        summary = _summary("SELECT * FROM orders LIMIT 9999", limit_cap=500)
        assert summary["limit_value"] == 9999
        assert summary["limit_clamped"] is True
        assert summary["limit_injected"] is False
        assert summary["normalized_sql"].endswith("LIMIT 500")

    def test_leaves_a_limit_below_the_cap_exactly_alone(self) -> None:
        # sqlglot's .limit() overwrites unconditionally, so calling it on every
        # statement would RAISE a generation's own LIMIT 10 to the cap and
        # return more rows than the query asked for.
        summary = _summary("SELECT * FROM orders LIMIT 10", limit_cap=500)
        assert summary["limit_value"] == 10
        assert summary["limit_injected"] is False
        assert summary["limit_clamped"] is False
        assert summary["normalized_sql"].endswith("LIMIT 10")

    def test_a_limit_equal_to_the_cap_is_untouched(self) -> None:
        summary = _summary("SELECT * FROM orders LIMIT 500", limit_cap=500)
        assert summary["limit_clamped"] is False
        assert summary["normalized_sql"].endswith("LIMIT 500")

    def test_a_non_constant_limit_is_reported_as_no_limit(self) -> None:
        # A LIMIT nothing can evaluate before execution is not a bound, so it
        # is reported as absent and the cap is applied over it.
        summary = _summary("SELECT * FROM orders LIMIT (SELECT COUNT(*) FROM orders)")
        assert summary["has_limit"] is False
        assert "limit_value" not in summary
        assert summary["limit_injected"] is True

    def test_preserves_an_offset_while_applying_the_cap(self) -> None:
        summary = _summary("SELECT * FROM orders LIMIT 9999 OFFSET 3", limit_cap=500)
        assert "LIMIT 500" in summary["normalized_sql"]
        assert "OFFSET 3" in summary["normalized_sql"]

    def test_applies_the_cap_to_a_cte_query(self) -> None:
        summary = _summary("WITH t AS (SELECT id FROM orders) SELECT * FROM t")
        assert summary["normalized_sql"].endswith("LIMIT 500")

    def test_applies_the_cap_to_a_union(self) -> None:
        summary = _summary("SELECT a FROM x UNION SELECT b FROM y")
        assert summary["normalized_sql"].endswith("LIMIT 500")

    def test_does_not_rewrite_a_stacked_query(self) -> None:
        # Rendering one would hand back something that looks executable, and
        # the guard is going to reject it on statement_count regardless.
        assert "normalized_sql" not in _summary("SELECT 1; DELETE FROM users")

    def test_does_not_rewrite_a_statement_that_has_no_row_limit(self) -> None:
        summary = _summary("DROP TABLE users")
        assert "normalized_sql" not in summary
        assert summary["has_limit"] is False


class TestUnparseable:
    @pytest.mark.parametrize(
        "sql",
        [
            "this is not sql at all !!!",
            "SELECT FROM WHERE",
            "SELECT * FROM (",
        ],
    )
    def test_reports_failure_rather_than_raising(self, sql: str) -> None:
        summary = _summary(sql)
        assert summary["ok"] is False
        assert summary["statement_count"] == 0
        assert summary["node_kinds"] == []
        assert summary["tables"] == []
        assert summary["functions"] == []
        assert summary["error"]

    def test_the_error_never_quotes_the_input(self) -> None:
        # sqlglot's own message embeds the offending SQL and a caret diagram of
        # it. That is model output on its way toward a browser.
        secret = "SELECT zzzsecretzzz FROM ((("
        assert "zzzsecretzzz" not in _summary(secret)["error"]

    def test_whitespace_only_input_is_a_failure_not_a_crash(self) -> None:
        assert _summary("   ")["ok"] is False


class TestValidateRequest:
    def test_accepts_a_well_formed_request(self) -> None:
        req = validate_request({"sql": "SELECT 1", "dialect": "sqlite", "limit_cap": 500})
        assert req == ParseRequest(sql="SELECT 1", dialect="sqlite", limit_cap=500)

    @pytest.mark.parametrize(
        ("payload", "because"),
        [
            ("not a dict", "body must be an object"),
            ({"dialect": "sqlite", "limit_cap": 1}, "sql is required"),
            ({"sql": "", "dialect": "sqlite", "limit_cap": 1}, "sql must be non-empty"),
            ({"sql": "   ", "dialect": "sqlite", "limit_cap": 1}, "sql must not be blank"),
            ({"sql": "x", "dialect": "mysql", "limit_cap": 1}, "dialect must be supported"),
            ({"sql": "x", "dialect": "sqlite"}, "limit_cap is required"),
            ({"sql": "x", "dialect": "sqlite", "limit_cap": 0}, "limit_cap below floor"),
            ({"sql": "x", "dialect": "sqlite", "limit_cap": 10001}, "limit_cap above ceiling"),
            ({"sql": "x", "dialect": "sqlite", "limit_cap": "500"}, "limit_cap must be an int"),
            # bool is an int in Python, so True would silently become a cap of 1.
            ({"sql": "x", "dialect": "sqlite", "limit_cap": True}, "a bool is not a cap"),
            ({"sql": "x", "dialect": "sqlite", "limit_cap": 1, "extra": 1}, "no unknown fields"),
        ],
    )
    def test_rejects_a_malformed_request(self, payload: object, because: str) -> None:
        with pytest.raises(ParseRequestError):
            validate_request(payload)

    def test_rejects_sql_beyond_the_contract_length(self) -> None:
        with pytest.raises(ParseRequestError):
            validate_request(
                {"sql": "x" * 20001, "dialect": "sqlite", "limit_cap": 500},
            )

    def test_the_rejection_message_never_quotes_the_sql(self) -> None:
        with pytest.raises(ParseRequestError) as caught:
            validate_request({"sql": "zzzsecretzzz", "dialect": "mysql", "limit_cap": 1})
        assert "zzzsecretzzz" not in str(caught.value)
