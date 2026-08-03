"""Transport-level tests for POST /v1/parse.

The parsing itself is covered in test_sqlguard_parse.py. What matters here is
the contract of the endpoint: which failures are 400s, which are 200s carrying
a negative result, and that nothing echoes the caller's SQL back out.
"""

from __future__ import annotations

from typing import Any

import pytest

from src.contracts import validate_document


def _post(client: Any, **body: Any) -> Any:
    return client.post("/v1/parse", json=body)


class TestSuccess:
    def test_summarises_a_plain_select(self, client: Any) -> None:
        response = _post(client, sql="SELECT * FROM orders", dialect="sqlite", limit_cap=500)
        assert response.status_code == 200
        body = response.json()
        validate_document("parse_summary.v1", body)
        assert body["ok"] is True
        assert body["tables"] == ["orders"]
        assert body["normalized_sql"].endswith("LIMIT 500")

    def test_every_response_conforms_to_the_contract(self, client: Any) -> None:
        # The guard fails closed on a document it cannot read, so a response
        # that violates its own contract takes the whole run down. Better to
        # find it in this service's tests than at the boundary.
        for sql in [
            "SELECT 1",
            "DROP TABLE users",
            "SELECT 1; DELETE FROM users",
            "not sql",
            "WITH t AS (SELECT 1) SELECT * FROM t",
            "SELECT pg_read_file('/etc/passwd')",
        ]:
            body = _post(client, sql=sql, dialect="sqlite", limit_cap=500).json()
            validate_document("parse_summary.v1", body)

    def test_the_postgres_dialect_is_served_too(self, client: Any) -> None:
        body = _post(client, sql="SELECT * FROM orders", dialect="postgres", limit_cap=10).json()
        assert body["dialect"] == "postgres"
        assert body["ok"] is True


class TestUnparseableIsNotAServerError:
    def test_answers_200_with_ok_false(self, client: Any) -> None:
        # A language model producing invalid SQL is the normal case this system
        # exists to handle. A 5xx would leave the guard unable to tell "your
        # SQL is wrong" from "the parser is down" — opposite situations, since
        # only the second should fail the run closed.
        response = _post(client, sql="this is not sql !!!", dialect="sqlite", limit_cap=500)
        assert response.status_code == 200
        body = response.json()
        assert body["ok"] is False
        assert body["error"]

    def test_the_error_does_not_echo_the_sql(self, client: Any) -> None:
        body = _post(
            client, sql="SELECT zzzsecretzzz FROM (((", dialect="sqlite", limit_cap=500
        ).json()
        assert "zzzsecretzzz" not in body["error"]


class TestBadRequest:
    @pytest.mark.parametrize(
        ("body", "because"),
        [
            ({"dialect": "sqlite", "limit_cap": 500}, "sql is required"),
            ({"sql": "", "dialect": "sqlite", "limit_cap": 500}, "sql must be non-empty"),
            ({"sql": "SELECT 1", "limit_cap": 500}, "dialect is required"),
            ({"sql": "SELECT 1", "dialect": "mysql", "limit_cap": 500}, "dialect unsupported"),
            ({"sql": "SELECT 1", "dialect": "sqlite"}, "limit_cap is required"),
            ({"sql": "SELECT 1", "dialect": "sqlite", "limit_cap": 0}, "cap below floor"),
            ({"sql": "SELECT 1", "dialect": "sqlite", "limit_cap": 99999}, "cap above ceiling"),
            ({"sql": "SELECT 1", "dialect": "sqlite", "limit_cap": 1, "x": 1}, "unknown field"),
        ],
    )
    def test_a_malformed_request_is_a_400(self, client: Any, body: dict, because: str) -> None:
        # Not a 200-with-ok-false: this is the runtime calling the service
        # wrongly, and no retry or repair can fix it.
        response = client.post("/v1/parse", json=body)
        assert response.status_code == 400, because
        assert "error" in response.json()

    def test_a_non_json_body_is_a_400(self, client: Any) -> None:
        response = client.post(
            "/v1/parse",
            content=b"{not json",
            headers={"Content-Type": "application/json"},
        )
        assert response.status_code == 400

    def test_a_json_scalar_body_is_a_400(self, client: Any) -> None:
        response = client.post("/v1/parse", json="SELECT 1")
        assert response.status_code == 400

    def test_the_400_does_not_echo_the_sql(self, client: Any) -> None:
        response = client.post(
            "/v1/parse",
            json={"sql": "zzzsecretzzz", "dialect": "mysql", "limit_cap": 500},
        )
        assert "zzzsecretzzz" not in response.text
