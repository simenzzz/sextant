"""Shared fixtures.

Deliberately small: a runtime factory that starts nothing, and a settings
builder. Everything else a test needs, it builds itself — shared mutable
fixtures are how test suites start depending on execution order.
"""

from __future__ import annotations

from typing import Any

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.config import Settings
from src.main import Runtime, create_app


async def noop_runtime(settings: Settings, app: FastAPI) -> Runtime | None:
    """A runtime factory that starts no infrastructure."""
    del settings, app
    return None


def make_settings(**overrides: Any) -> Settings:
    """Build Settings without reading the environment."""
    return Settings(**overrides)


@pytest.fixture
def client() -> Any:
    """A TestClient over the real app with a no-op runtime.

    Used as a context manager so FastAPI's lifespan actually runs — without
    that, /ready would never flip and the readiness tests would be vacuous.
    """
    app = create_app(make_settings(), runtime_factory=noop_runtime)
    with TestClient(app) as c:
        yield c
