"""Validate documents against the shared JSON Schema contracts.

The mirror of the Go runtime's internal/contracts package, and it works the
same way: the schemas come from src/models/gen/schemas, which codegen copies
verbatim from packages/contracts/schemas, so this service validates against
exactly the files its generated models were built from. The CI drift gate is
what keeps that copy honest.

House rule this serves: validate at every boundary — in on the way in, out on
the way out.
"""

from __future__ import annotations

import json
from functools import cache
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator
from jsonschema import ValidationError as JsonSchemaValidationError

SCHEMA_DIR = Path(__file__).resolve().parent / "models" / "gen" / "schemas"

SCHEMA_SUFFIX = ".schema.json"


class ContractError(Exception):
    """Base for contract failures."""


class UnknownContractError(ContractError):
    """No embedded schema exists for the requested name."""


class DocumentInvalidError(ContractError):
    """A document did not conform to its contract.

    ``detail`` quotes the offending value and is for the log. ``safe_message``
    is what may cross a service boundary: a failing document is often model or
    driver output, and echoing it onward is how untrusted content ends up in a
    browser.
    """

    def __init__(self, name: str, detail: str) -> None:
        super().__init__(f"{name}: {detail}")
        self.name = name
        self.detail = detail

    @property
    def safe_message(self) -> str:
        return f"{self.name}: document failed validation"


@cache
def _validator(name: str) -> Draft202012Validator:
    path = SCHEMA_DIR / f"{name}{SCHEMA_SUFFIX}"
    if not path.is_file():
        raise UnknownContractError(f"unknown contract {name!r}")
    schema = json.loads(path.read_text())
    # format is an assertion, not an annotation. All three language suites run
    # the same fixture corpus, so they have to agree on whether "last tuesday"
    # is a date-time; leaving formats as annotations would make this service
    # accept documents the Go runtime rejects.
    return Draft202012Validator(schema, format_checker=Draft202012Validator.FORMAT_CHECKER)


def contract_names() -> list[str]:
    """Every contract this build can validate."""
    return sorted(p.name.removesuffix(SCHEMA_SUFFIX) for p in SCHEMA_DIR.glob(f"*{SCHEMA_SUFFIX}"))


def validate_document(name: str, document: Any) -> None:
    """Check a document against a named contract.

    Raises:
        UnknownContractError: the name has no embedded schema.
        DocumentInvalidError: the document does not conform.
    """
    validator = _validator(name)
    try:
        validator.validate(document)
    except JsonSchemaValidationError as exc:
        raise DocumentInvalidError(name, exc.message) from exc
