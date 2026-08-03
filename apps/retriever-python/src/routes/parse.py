"""The /v1/parse route.

Transport only: validate in, summarize, validate out. All parsing logic lives
in src.sqlguard, and all policy lives in the Go guard that consumes this.
"""

from __future__ import annotations

import json
import logging
from typing import Any

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse

from src.contracts import ContractError, validate_document
from src.sqlguard.parse import ParseRequestError, summarize, validate_request

logger = logging.getLogger(__name__)

router = APIRouter()

# MAX_REQUEST_BYTES bounds the whole request body.
#
# A conforming request is small: question_request.v1 caps the SQL at 20000
# characters. This bounds what a non-conforming one can make the service
# allocate before it finds that out.
MAX_REQUEST_BYTES = 256 * 1024


def _too_large() -> JSONResponse:
    return JSONResponse({"error": "request body is too large"}, status_code=413)


@router.post("/v1/parse")
async def parse(request: Request) -> JSONResponse:
    """Summarize one candidate SQL statement for the runtime's guard.

    An unparseable statement is answered 200 with ``ok`` false, not 5xx. A
    language model producing invalid SQL is the normal case this system exists
    to handle, and turning it into a server error would make the guard unable
    to tell "your SQL is wrong" from "the parser is down" — which are opposite
    situations, since only the second should fail the run closed.

    A malformed *request* is a 400: that is the runtime calling this service
    wrongly, and it is not something a retry or a repair can fix.
    """
    # Bounded BEFORE the read. validate_request enforces MAX_SQL_CHARS, but it
    # runs after the body is already in memory, so the size check would not
    # actually bound the resource. The endpoint is reachable unauthenticated
    # from anything on the compose network.
    declared = request.headers.get("content-length")
    if declared is not None:
        try:
            if int(declared) > MAX_REQUEST_BYTES:
                return _too_large()
        except ValueError:
            return _bad_request("content-length must be an integer")

    body = b""
    async for chunk in request.stream():
        body += chunk
        # A chunked request can omit content-length entirely, so the running
        # total is the check that actually holds.
        if len(body) > MAX_REQUEST_BYTES:
            return _too_large()

    try:
        payload = json.loads(body)
    except Exception:
        return _bad_request("body must be valid JSON")

    try:
        parse_request = validate_request(payload)
    except ParseRequestError as exc:
        return _bad_request(str(exc))

    try:
        summary: dict[str, Any] = summarize(parse_request)
    except Exception:
        # summarize promises not to raise. If it ever does, that is our bug and
        # it belongs in the log as one — not forwarded to a caller that would
        # read a 500 as "the parser is down" and fail an otherwise fine run
        # closed.
        logger.exception("summarize raised despite promising not to")
        return JSONResponse({"error": "internal error summarizing the statement"}, status_code=500)

    # Validated on the way out as well as in. The guard fails closed on a
    # document it cannot read, so emitting one that violates the contract would
    # take the whole run down — better to find it here, in this service's own
    # tests, than at the boundary.
    try:
        validate_document("parse_summary.v1", summary)
    except ContractError:
        logger.exception("built a parse summary that violates its own contract")
        return JSONResponse(
            {"error": "internal error building the parse summary"},
            status_code=500,
        )

    return JSONResponse(summary, status_code=200)


def _bad_request(detail: str) -> JSONResponse:
    """A 400 whose message is safe to return.

    Request-shape complaints never quote the SQL: it is model output on its way
    back toward a browser, and the runtime logs the statement server-side.
    """
    return JSONResponse({"error": detail}, status_code=400)
