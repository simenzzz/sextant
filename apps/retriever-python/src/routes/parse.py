"""The /v1/parse route.

Transport only: validate in, summarize, validate out. All parsing logic lives
in src.sqlguard, and all policy lives in the Go guard that consumes this.
"""

from __future__ import annotations

import logging
from typing import Any

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse

from src.contracts import ContractError, validate_document
from src.sqlguard.parse import ParseRequestError, summarize, validate_request

logger = logging.getLogger(__name__)

router = APIRouter()


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
    try:
        payload = await request.json()
    except Exception:
        return _bad_request("body must be valid JSON")

    try:
        parse_request = validate_request(payload)
    except ParseRequestError as exc:
        return _bad_request(str(exc))

    summary: dict[str, Any] = summarize(parse_request)

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
