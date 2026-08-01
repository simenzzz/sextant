"""Runs the shared cross-language contract fixture corpus.

The same fixture files are executed by the Go and TypeScript suites; a schema
change without fixture updates fails all three at once instead of drifting
quietly in one language.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator

REPO_ROOT = Path(__file__).resolve().parents[3]
# The service's own embedded copy, matching what Go embeds and TypeScript
# imports. Reading the canonical packages/contracts/schemas here instead would
# mean this suite validates a file the running service never loads — and the
# embedded copy would go untested in every language.
SCHEMAS = Path(__file__).resolve().parents[1] / "src" / "models" / "gen" / "schemas"
# The corpus is deliberately shared across all three languages, so it stays at
# the repo root.
FIXTURES = REPO_ROOT / "packages" / "contracts" / "fixtures"


def _validator(schema_name: str) -> Draft202012Validator:
    schema = json.loads((SCHEMAS / f"{schema_name}.schema.json").read_text())
    # format is an assertion here, not an annotation — all three languages must
    # agree on what the corpus accepts. Requires rfc3339-validator and rfc3987
    # (dev deps); without them FormatChecker silently skips those formats and
    # this suite starts accepting fixtures the Go suite rejects.
    return Draft202012Validator(schema, format_checker=Draft202012Validator.FORMAT_CHECKER)


def _cases() -> list[tuple[str, str, Path]]:
    cases: list[tuple[str, str, Path]] = []
    for schema_dir in sorted(FIXTURES.iterdir()):
        if not schema_dir.is_dir():
            continue
        for kind in ("valid", "invalid"):
            files = sorted((schema_dir / kind).glob("*.json"))
            assert files, f"{schema_dir.name}: {kind}/ is empty"
            cases.extend((schema_dir.name, kind, f) for f in files)
    assert cases, "fixture corpus is empty"
    return cases


@pytest.mark.parametrize(
    ("schema_name", "kind", "path"),
    _cases(),
    ids=lambda v: v.name if isinstance(v, Path) else str(v),
)
def test_shared_fixture_corpus(schema_name: str, kind: str, path: Path) -> None:
    validator = _validator(schema_name)
    doc = json.loads(path.read_text())
    errors = list(validator.iter_errors(doc))
    if kind == "valid":
        assert not errors, f"{path.name} expected valid, got: {errors[0].message}"
    else:
        assert errors, f"{path.name} expected a validation failure, got none"


def test_format_assertion_is_actually_enabled() -> None:
    """Guards the guard.

    If rfc3339-validator drops out of the dev extras, FormatChecker stops
    asserting date-time and the format fixtures above pass vacuously. This
    fails loudly instead.
    """
    validator = _validator("eval_result.v1")
    doc = json.loads((FIXTURES / "eval_result.v1" / "valid" / "minimal.json").read_text())
    doc["started_at"] = "last tuesday"
    assert list(validator.iter_errors(doc)), (
        "date-time format is not being asserted — install the rfc3339-validator dev dependency"
    )


def test_generated_pydantic_model_parses_valid_fixture() -> None:
    """The generated typed layer must accept what the schema layer accepts."""
    from src.models.gen.sql_plan_v1_schema import SqlPlanV1

    doc = json.loads((FIXTURES / "sql_plan.v1" / "valid" / "minimal.json").read_text())
    plan = SqlPlanV1.model_validate(doc)

    # Constrained array items come back as RootModel wrappers, not bare
    # strings: datamodel-codegen turns `items: {type: string, minLength: 1}`
    # into its own model so the constraint survives. Anything consuming
    # plan.tables has to unwrap .root — asserted here so the shape is
    # documented rather than discovered at a call site.
    assert [t.root for t in plan.tables] == ["orders"]
    assert plan.limit_value == 100


def test_embedded_schemas_match_the_source_of_truth() -> None:
    """The embedded copy is only trustworthy if codegen actually refreshed it.

    The CI drift gate enforces this too, but asserting it here means a stale
    copy fails the suite locally rather than only on push.
    """
    canonical = REPO_ROOT / "packages" / "contracts" / "schemas"
    embedded = sorted(p.name for p in SCHEMAS.glob("*.schema.json"))
    source = sorted(p.name for p in canonical.glob("*.schema.json"))
    assert embedded == source, "embedded schema set differs from packages/contracts/schemas"

    for name in embedded:
        assert (SCHEMAS / name).read_text() == (canonical / name).read_text(), (
            f"{name} drifted from the source of truth — run `make generate-schemas`"
        )
