"""FastAPI application factory for the retriever sidecar.

The factory takes an injectable runtime factory so tests can build the real
app with no infrastructure behind it. Without that seam, every test either
starts an embedding model or monkey-patches module globals.
"""

from __future__ import annotations

from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from typing import Protocol

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

from src.config import Settings, load_settings
from src.routes.parse import router as parse_router

SERVICE_NAME = "retriever-python"
SERVICE_VERSION = "0.1.0"


class Runtime(Protocol):
    """Whatever the app holds open for its lifetime.

    At P0 nothing implements this. At P5 it is the loaded embedding model and
    the schema index; the Protocol exists now so the lifespan wiring does not
    have to change when it arrives.
    """

    async def aclose(self) -> None: ...


RuntimeFactory = Callable[[Settings, FastAPI], Awaitable[Runtime | None]]


async def start_runtime(settings: Settings, app: FastAPI) -> Runtime | None:
    """Default runtime factory.

    Returns None at P0: there is nothing to load yet, and pretending otherwise
    would make /ready lie about readiness.
    """
    del settings, app  # nothing to build yet
    return None


class AppState:
    """Mutable runtime state, deliberately tiny and owned by the app instance."""

    def __init__(self, settings: Settings) -> None:
        self.settings = settings
        self.ready = False


def create_app(
    settings: Settings | None = None,
    *,
    runtime_factory: RuntimeFactory = start_runtime,
) -> FastAPI:
    """Build the application.

    Args:
        settings: validated settings; loaded from the environment when omitted.
        runtime_factory: builds whatever the app holds open. Tests pass a no-op
            so the real app can be exercised without infrastructure.
    """
    resolved = settings or load_settings()

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        runtime = await runtime_factory(resolved, app)
        app.state.ctx.ready = True
        try:
            yield
        finally:
            # Cleared before the close so a shutdown that hangs still reports
            # not-ready rather than advertising a service that is going away.
            app.state.ctx.ready = False
            if runtime is not None:
                await runtime.aclose()

    app = FastAPI(title="Sextant Retriever", version=SERVICE_VERSION, lifespan=lifespan)
    app.state.ctx = AppState(resolved)

    # The allowlist is wired, not merely parsed. A config knob that looks like
    # a security control but enforces nothing is worse than no knob: it invites
    # someone debugging a blocked request to reach for a wildcard.
    #
    # An empty allowlist installs no middleware, so no cross-origin request is
    # permitted — the safe reading of an unset variable. allow_origins is never
    # ["*"], and credentials are never allowed.
    if resolved.allowed_origins:
        app.add_middleware(
            CORSMiddleware,
            allow_origins=list(resolved.allowed_origins),
            allow_credentials=False,
            allow_methods=["GET", "POST", "OPTIONS"],
            allow_headers=["Content-Type"],
            max_age=600,
        )

    # Mounted inside the CORS wrapper above, so a new route cannot be added
    # without the origin allowlist applying to it. /v1/parse is called by the
    # Go runtime over the private compose network rather than by a browser,
    # but that is a deployment fact and not something to rely on.
    app.include_router(parse_router)

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        """Liveness: the process is up. Says nothing about readiness."""
        return {"service": SERVICE_NAME, "status": "ok"}

    @app.get("/ready")
    def ready() -> JSONResponse:
        """Readiness: the runtime finished loading and requests can be served."""
        is_ready: bool = app.state.ctx.ready
        return JSONResponse(
            {"service": SERVICE_NAME, "ready": is_ready},
            status_code=200 if is_ready else 503,
        )

    return app
