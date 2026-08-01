#!/usr/bin/env bash
# Rebuilds infra/fixtures/toy.sqlite from infra/fixtures/toy.sql.
#
# The .sqlite file is committed (see .gitignore's explicit exception) so tests
# and CI run with zero downloads. The .sql file is the source of truth: edit
# that, run this, commit both.
#
# Uses Python's stdlib sqlite3 rather than the sqlite3 CLI, which is not
# installed by default on this machine or on GitHub's runners.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SQL="$ROOT/infra/fixtures/toy.sql"
DB="$ROOT/infra/fixtures/toy.sqlite"

command -v python3 >/dev/null 2>&1 || { echo "ERROR: python3 not found" >&2; exit 1; }

rm -f "$DB"
python3 - "$SQL" "$DB" <<'PY'
import sqlite3
import sys

sql_path, db_path = sys.argv[1], sys.argv[2]
with open(sql_path, encoding="utf-8") as fh:
    script = fh.read()

conn = sqlite3.connect(db_path)
try:
    conn.executescript(script)
    conn.commit()
    tables = [
        row[0]
        for row in conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
        )
    ]
finally:
    conn.close()

print(f"built {db_path}")
for t in tables:
    print(f"  table: {t}")
PY
