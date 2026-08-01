"""Uvicorn entrypoint.

Kept separate from the factory so tests import create_app without importing
uvicorn or binding a port.
"""

from __future__ import annotations

import uvicorn

from src.config import load_settings
from src.main import create_app

app = create_app()


def main() -> None:  # pragma: no cover - process entrypoint
    settings = load_settings()
    uvicorn.run(app, host=settings.host, port=settings.port)


if __name__ == "__main__":  # pragma: no cover - process entrypoint
    main()
