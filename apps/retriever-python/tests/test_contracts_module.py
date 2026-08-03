"""Tests for the runtime contract validator.

The mirror of the Go runtime's internal/contracts tests: a name with no schema
is a distinct failure from a document that does not conform, because callers
handle them differently — the first is a bug, the second is input.
"""

from __future__ import annotations

import pytest

from src.contracts import (
    DocumentInvalidError,
    UnknownContractError,
    contract_names,
    validate_document,
)


def test_lists_every_embedded_contract() -> None:
    names = contract_names()
    assert "parse_summary.v1" in names
    assert "result_set.v1" in names
    # The set is copied verbatim from packages/contracts/schemas by codegen, so
    # a schema added there without regenerating shows up as a shortfall here.
    assert len(names) == 7


def test_accepts_a_conforming_document() -> None:
    validate_document(
        "parse_summary.v1",
        {
            "schema": "parse_summary.v1",
            "ok": True,
            "dialect": "sqlite",
            "statement_count": 1,
            "node_kinds": ["Select"],
            "tables": ["orders"],
            "functions": [],
        },
    )


def test_an_unknown_contract_is_its_own_error() -> None:
    with pytest.raises(UnknownContractError):
        validate_document("not_a_contract.v9", {})


def test_a_nonconforming_document_reports_where_it_failed() -> None:
    with pytest.raises(DocumentInvalidError) as caught:
        validate_document("parse_summary.v1", {"schema": "parse_summary.v1"})
    assert caught.value.name == "parse_summary.v1"
    assert caught.value.detail


def test_the_safe_message_does_not_quote_the_document() -> None:
    # A failing document is often model or driver output. detail is for the
    # log; safe_message is what may cross a service boundary.
    with pytest.raises(DocumentInvalidError) as caught:
        validate_document(
            "result_set.v1",
            {"schema": "result_set.v1", "run_id": "zzzsecretzzz"},
        )
    assert "zzzsecretzzz" not in caught.value.safe_message


def test_format_is_asserted_not_merely_annotated() -> None:
    # All three language suites run the same fixture corpus, so they have to
    # agree on what a date is. Without the format checker installed this passes
    # silently and the corpus stops meaning the same thing in every language.
    with pytest.raises(DocumentInvalidError):
        validate_document(
            "cost_ledger.v1",
            {
                "schema": "cost_ledger.v1",
                "run_id": "r_1",
                "price_table_date": "last tuesday",
                "entries": [],
                "totals": {"tokens_in": 0, "tokens_out": 0, "usd": 0, "ms": 0},
            },
        )
