"""Liveness and readiness."""

from __future__ import annotations

from fastapi.testclient import TestClient

from src.main import SERVICE_NAME, create_app
from tests.conftest import make_settings, noop_runtime


def test_healthz(client: TestClient) -> None:
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {"service": SERVICE_NAME, "status": "ok"}


def test_ready_once_lifespan_has_run(client: TestClient) -> None:
    response = client.get("/ready")
    assert response.status_code == 200
    assert response.json() == {"service": SERVICE_NAME, "ready": True}


def test_ready_is_503_before_lifespan_runs() -> None:
    """Readiness must not be true just because the process started.

    A TestClient used without the context manager never runs lifespan, which
    is the same state the app is in while a model is still loading.
    """
    app = create_app(make_settings(), runtime_factory=noop_runtime)
    response = TestClient(app).get("/ready")
    assert response.status_code == 503
    assert response.json()["ready"] is False


def test_runtime_is_closed_on_shutdown() -> None:
    """A runtime that was opened must be closed when the app stops."""
    closed = False

    class Recording:
        async def aclose(self) -> None:
            nonlocal closed
            closed = True

    async def factory(settings: object, app: object) -> Recording:
        del settings, app
        return Recording()

    app = create_app(make_settings(), runtime_factory=factory)  # type: ignore[arg-type]
    with TestClient(app) as c:
        assert c.get("/ready").status_code == 200
    assert closed, "the runtime was not closed on shutdown — this leaks on every restart"
