#!/usr/bin/env bash
# Regenerates contract artifacts for all three languages from
# packages/contracts/schemas (the single source of truth).
#
# Outputs (all committed, all covered by the CI drift gate):
#   apps/runtime-go/internal/contracts/gen/*.go       Go structs
#   apps/retriever-python/src/models/gen/*.py         Pydantic v2 models
#   apps/web/src/contracts/gen/*.d.ts                 TypeScript types
#   */gen/schemas/*.schema.json                       verbatim schema copies,
#       embedded by each service so runtime validation and the generated types
#       can never disagree about which schema they came from.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SCHEMAS="$ROOT/packages/contracts/schemas"
CODEGEN="$ROOT/packages/contracts/codegen"

# shellcheck source=versions.env
source "$CODEGEN/versions.env"

GO_OUT="$ROOT/apps/runtime-go/internal/contracts/gen"
PY_OUT="$ROOT/apps/retriever-python/src/models/gen"
TS_OUT="$ROOT/apps/web/src/contracts/gen"

GOJSONSCHEMA="${GOJSONSCHEMA:-$HOME/go/bin/go-jsonschema}"
DATAMODEL_CODEGEN="${DATAMODEL_CODEGEN:-$CODEGEN/.venv/bin/datamodel-codegen}"

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "ERROR: missing tool: $1 ($2)" >&2; exit 1; }
}
# Resolved to the local install rather than invoked through npx. `npx json2ts`
# falls back to fetching a registry package literally named "json2ts" when the
# local node_modules is missing — which is NOT the declared dependency
# (json-schema-to-typescript), and would be a typosquat window on the tool that
# writes committed source into all three services.
JSON2TS="$CODEGEN/node_modules/.bin/json2ts"
[[ -x "$JSON2TS" ]] || {
  echo "ERROR: missing $JSON2TS (npm ci in packages/contracts/codegen, or: make setup-codegen)" >&2
  exit 1
}
[[ -x "$GOJSONSCHEMA" ]] || {
  echo "ERROR: missing $GOJSONSCHEMA (go install $GO_JSONSCHEMA_MODULE@$GO_JSONSCHEMA_VERSION, or: make setup-codegen)" >&2
  exit 1
}
[[ -x "$DATAMODEL_CODEGEN" ]] || {
  echo "ERROR: missing $DATAMODEL_CODEGEN (make setup-codegen)" >&2
  exit 1
}

# Refuse to generate with a drifted toolchain. Output differs across generator
# versions, so generating with the wrong one produces a diff that looks like a
# schema change and isn't. Fail loudly here instead of confusing the drift gate.
# An unreadable version fails rather than skipping the check. Skipping would
# silently disable the guard the moment a tool changes its --version format,
# and the drift gate would then absorb the confusing diff.
actual_dmc="$("$DATAMODEL_CODEGEN" --version 2>/dev/null | awk '{print $NF}')" || true
[[ -n "$actual_dmc" ]] || { echo "ERROR: cannot read datamodel-codegen version" >&2; exit 1; }
if [[ "$actual_dmc" != "$DATAMODEL_CODEGEN_VERSION" ]]; then
  echo "ERROR: datamodel-codegen $actual_dmc installed, need $DATAMODEL_CODEGEN_VERSION (see versions.env)" >&2
  exit 1
fi
# go-jsonschema ships no --version flag, so read the module version Go embeds
# in the binary instead. This is stricter than a flag would be: it reports what
# was actually built, not what the tool claims.
actual_gjs="$(go version -m "$GOJSONSCHEMA" 2>/dev/null | awk '$1=="mod"{print $3}')" || true
[[ -n "$actual_gjs" ]] || { echo "ERROR: cannot read go-jsonschema version from $GOJSONSCHEMA" >&2; exit 1; }
if [[ "$actual_gjs" != "$GO_JSONSCHEMA_VERSION" ]]; then
  echo "ERROR: go-jsonschema $actual_gjs installed, need $GO_JSONSCHEMA_VERSION (see versions.env)" >&2
  exit 1
fi

mkdir -p "$GO_OUT" "$PY_OUT" "$TS_OUT"

# Stale outputs are removed, not just overwritten. Without this, deleting a
# schema leaves its generated type tracked and unchanged, so `git diff` sees
# nothing and the drift gate passes while a dead type keeps compiling forever.
rm -f "$GO_OUT"/*.go "$PY_OUT"/*.py "$TS_OUT"/*.d.ts

# ---- verbatim schema copies for runtime validation (each service embeds its own) ----
for out in "$GO_OUT" "$PY_OUT" "$TS_OUT"; do
  rm -rf "${out:?}/schemas"
  mkdir -p "$out/schemas"
  cp "$SCHEMAS"/*.schema.json "$out/schemas/"
done

schema_files=("$SCHEMAS"/*.schema.json)

# ---- Go (go-jsonschema) ----
for f in "${schema_files[@]}"; do
  base="$(basename "$f" .schema.json)"           # e.g. trace_event.v1
  gofile="$GO_OUT/$(echo "$base" | tr '.' '_').go"
  "$GOJSONSCHEMA" -p gen --only-models --struct-name-from-title -o "$gofile" "$f"
done

# ---- Python (datamodel-code-generator -> Pydantic v2) ----
# --formatters builtin is deliberate. The tool's default (black + isort) pulls
# in two formatters that versions.env does not pin, so a black release would
# reformat the output and trip the drift gate with no schema change behind it.
# The builtin formatter has no external dependency and therefore no such drift.
#
# One coupling to know about: the builtin formatter reads `line-length` from the
# nearest pyproject.toml, which here is apps/retriever-python/pyproject.toml's
# [tool.ruff] section. Changing that value reflows every generated model, so it
# must be regenerated in the same commit or the drift gate fires with no schema
# change behind it. Verified by changing the value and observing the diff.
"$DATAMODEL_CODEGEN" \
  --input "$SCHEMAS" \
  --input-file-type jsonschema \
  --output "$PY_OUT" \
  --output-model-type pydantic_v2.BaseModel \
  --disable-timestamp \
  --use-schema-description \
  --target-python-version 3.12 \
  --formatters builtin
touch "$PY_OUT/__init__.py"

# ---- TypeScript (json-schema-to-typescript) ----
for f in "${schema_files[@]}"; do
  base="$(basename "$f" .schema.json)"
  "$JSON2TS" "$f" \
    --bannerComment "/* GENERATED by packages/contracts/codegen/generate.sh — DO NOT EDIT */" \
    --additionalProperties false > "$TS_OUT/${base}.d.ts"
done

echo "contracts: generated ${#schema_files[@]} schema(s) for go/py/ts"
