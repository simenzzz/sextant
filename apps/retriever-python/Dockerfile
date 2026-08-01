FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
WORKDIR /app

COPY pyproject.toml ./
COPY src ./src
RUN pip install --no-cache-dir .

RUN useradd --system --uid 10001 sextant
USER sextant
EXPOSE 8000
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://localhost:8000/healthz')" || exit 1
# `python -m src.server` rather than a uvicorn command line: server.main()
# reads host and port from settings, so SEXTANT_RETRIEVER_PORT actually
# takes effect. Hardcoding the port here would make that variable a no-op
# in the only environment that ships.
CMD ["python", "-m", "src.server"]
