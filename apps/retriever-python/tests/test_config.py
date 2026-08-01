"""Settings loading.

The rule under test throughout: unset takes the default, set-but-invalid is an
error. A cost or retrieval cap that silently reverts to its default because of
a typo is a cap that has stopped existing.
"""

from __future__ import annotations

import pytest

from src.config import (
    DEFAULT_FK_HOPS,
    DEFAULT_PORT,
    DEFAULT_TABLE_BUDGET,
    DEFAULT_TOP_K,
    ConfigError,
    _parse_origins,
    load_settings,
)

ENV_VARS = (
    "SEXTANT_RETRIEVER_HOST",
    "SEXTANT_RETRIEVER_PORT",
    "SEXTANT_EMBEDDING_MODEL",
    "SEXTANT_TOP_K",
    "SEXTANT_TABLE_BUDGET",
    "SEXTANT_FK_HOPS",
    "SEXTANT_ALLOWED_ORIGINS",
)


@pytest.fixture(autouse=True)
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Start from a known environment regardless of the developer's shell."""
    for key in ENV_VARS:
        monkeypatch.delenv(key, raising=False)


def test_defaults() -> None:
    settings = load_settings()
    assert settings.port == DEFAULT_PORT
    assert settings.top_k == DEFAULT_TOP_K
    assert settings.table_budget == DEFAULT_TABLE_BUDGET
    assert settings.fk_hops == DEFAULT_FK_HOPS
    assert settings.allowed_origins == ()


def test_overrides(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SEXTANT_TOP_K", "5")
    monkeypatch.setenv("SEXTANT_TABLE_BUDGET", "30")
    monkeypatch.setenv("SEXTANT_FK_HOPS", "2")
    monkeypatch.setenv("SEXTANT_ALLOWED_ORIGINS", "https://a.example, ,https://b.example")

    settings = load_settings()
    assert settings.top_k == 5
    assert settings.table_budget == 30
    assert settings.fk_hops == 2
    assert settings.allowed_origins == ("https://a.example", "https://b.example")


@pytest.mark.parametrize(
    ("key", "value", "expected_substring"),
    [
        ("SEXTANT_TOP_K", "ten", "invalid integer"),
        ("SEXTANT_TOP_K", "0", "must be in [1,100]"),
        ("SEXTANT_TOP_K", "1000", "must be in [1,100]"),
        ("SEXTANT_FK_HOPS", "9", "must be in [0,3]"),
        ("SEXTANT_TABLE_BUDGET", "0", "must be in [1,200]"),
        ("SEXTANT_RETRIEVER_PORT", "70000", "must be in [1,65535]"),
        ("SEXTANT_RETRIEVER_PORT", "eight thousand", "invalid integer"),
    ],
)
def test_rejects_invalid_values(
    monkeypatch: pytest.MonkeyPatch, key: str, value: str, expected_substring: str
) -> None:
    monkeypatch.setenv(key, value)
    with pytest.raises(ConfigError) as excinfo:
        load_settings()
    assert expected_substring in str(excinfo.value)
    assert key in str(excinfo.value), "the error must name the variable that is wrong"


def test_budget_below_top_k_is_rejected(monkeypatch: pytest.MonkeyPatch) -> None:
    """A budget under top_k would discard tables similarity already chose.

    That is a silently degraded retriever, so it is a configuration error
    rather than something to clamp quietly.
    """
    monkeypatch.setenv("SEXTANT_TOP_K", "20")
    monkeypatch.setenv("SEXTANT_TABLE_BUDGET", "5")
    with pytest.raises(ConfigError, match="below SEXTANT_TOP_K"):
        load_settings()


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        ("", ()),
        ("   ", ()),
        ("https://a.example", ("https://a.example",)),
        ("https://a.example,,  ,https://b.example ", ("https://a.example", "https://b.example")),
    ],
)
def test_parse_origins(raw: str, expected: tuple[str, ...]) -> None:
    assert _parse_origins(raw) == expected
