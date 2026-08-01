"""Settings for the retriever sidecar.

Same rule as the Go runtime's config package: an unset variable takes its
default, a variable that is set but unparseable is an error. A silent fallback
turns a typo into a behaviour change nobody sees.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

DEFAULT_HOST = "0.0.0.0"  # noqa: S104 - container services bind all interfaces
DEFAULT_PORT = 8000
DEFAULT_EMBEDDING_MODEL = "all-MiniLM-L6-v2"
DEFAULT_TOP_K = 10
DEFAULT_TABLE_BUDGET = 20
DEFAULT_FK_HOPS = 1

MAX_TOP_K = 100
MAX_TABLE_BUDGET = 200
MAX_FK_HOPS = 3


class ConfigError(ValueError):
    """Raised when an environment variable is set but unusable."""


@dataclass(frozen=True)
class Settings:
    """Validated configuration. Frozen: nothing rebinds these after load."""

    host: str = DEFAULT_HOST
    port: int = DEFAULT_PORT
    embedding_model: str = DEFAULT_EMBEDDING_MODEL
    top_k: int = DEFAULT_TOP_K
    table_budget: int = DEFAULT_TABLE_BUDGET
    fk_hops: int = DEFAULT_FK_HOPS
    allowed_origins: tuple[str, ...] = field(default=())


def _env_int(key: str, default: int) -> int:
    raw = os.getenv(key, "")
    if not raw.strip():
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise ConfigError(f"{key}: invalid integer {raw!r}") from exc


def _parse_origins(raw: str) -> tuple[str, ...]:
    """Split a comma-separated allowlist, dropping blanks.

    An empty result means no cross-origin browser client is allowed, which is
    the safe reading of an unset variable — never "allow everything".
    """
    return tuple(part.strip() for part in raw.split(",") if part.strip())


def load_settings() -> Settings:
    """Read settings from the environment, validating ranges."""
    settings = Settings(
        host=os.getenv("SEXTANT_RETRIEVER_HOST", "").strip() or DEFAULT_HOST,
        port=_env_int("SEXTANT_RETRIEVER_PORT", DEFAULT_PORT),
        embedding_model=os.getenv("SEXTANT_EMBEDDING_MODEL", "").strip() or DEFAULT_EMBEDDING_MODEL,
        top_k=_env_int("SEXTANT_TOP_K", DEFAULT_TOP_K),
        table_budget=_env_int("SEXTANT_TABLE_BUDGET", DEFAULT_TABLE_BUDGET),
        fk_hops=_env_int("SEXTANT_FK_HOPS", DEFAULT_FK_HOPS),
        allowed_origins=_parse_origins(os.getenv("SEXTANT_ALLOWED_ORIGINS", "")),
    )

    if not 1 <= settings.port <= 65535:
        raise ConfigError(f"SEXTANT_RETRIEVER_PORT: must be in [1,65535], got {settings.port}")
    if not 1 <= settings.top_k <= MAX_TOP_K:
        raise ConfigError(f"SEXTANT_TOP_K: must be in [1,{MAX_TOP_K}], got {settings.top_k}")
    if not 1 <= settings.table_budget <= MAX_TABLE_BUDGET:
        raise ConfigError(
            f"SEXTANT_TABLE_BUDGET: must be in [1,{MAX_TABLE_BUDGET}], got {settings.table_budget}"
        )
    if not 0 <= settings.fk_hops <= MAX_FK_HOPS:
        raise ConfigError(f"SEXTANT_FK_HOPS: must be in [0,{MAX_FK_HOPS}], got {settings.fk_hops}")
    if settings.table_budget < settings.top_k:
        raise ConfigError(
            f"SEXTANT_TABLE_BUDGET ({settings.table_budget}) is below SEXTANT_TOP_K "
            f"({settings.top_k}): the budget would discard tables similarity already selected"
        )
    return settings
