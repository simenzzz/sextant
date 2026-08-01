#!/usr/bin/env bash
# Fetches the BIRD dev set into eval/data/ (gitignored).
#
# This is a manual, deliberate step. It is NOT run by CI, `make test`, or
# `docker compose up` — the whole pipeline stays green against
# infra/fixtures/toy.sqlite with zero downloads. Run this when you reach P2
# and need real benchmark questions.
#
# Two things this script will not do for you:
#   1. Accept the license on your behalf. It prints where the terms live and
#      requires you to type "yes".
#   2. Tell you how many questions are in the dev set. PLAN.md 6.1 is explicit
#      that the counts and license terms are verified at download time rather
#      than quoted from a document — so read what you actually downloaded.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEST="$ROOT/eval/data"

BIRD_HOME="https://bird-bench.github.io/"
# Verify this against the project page before trusting it: benchmark hosts move
# their artifacts, and a silently-404ing URL is better than a silently-wrong one.
BIRD_DEV_URL="${BIRD_DEV_URL:-https://bird-bench.oss-cn-beijing.aliyuncs.com/dev.zip}"

# Expected SHA-256 of the archive. Deliberately empty: nobody has downloaded it
# yet, and inventing a digest would be worse than having none. On the first run
# the script prints what it received and stops; commit that digest here, in the
# same commit that records the real question counts in PLAN.md section 6.1.
#
# Without this, a bucket that lapses or is taken over — which the URL comment
# above already anticipates — silently substitutes the archive that gets
# unzipped on the machine holding the paid API key.
BIRD_DEV_SHA256="${BIRD_DEV_SHA256:-}"

cat <<EOF
BIRD dev set download
---------------------
  Project page : $BIRD_HOME
  Archive      : $BIRD_DEV_URL
  Destination  : $DEST

Before continuing:
  - Read the license and terms of use on the project page. BIRD is a research
    benchmark; redistribution and commercial use are restricted.
  - The archive is multi-gigabyte. Check free space first:
      df -h "$ROOT"
  - Nothing downloaded here is ever committed: eval/data/ is gitignored.

EOF

read -r -p 'Have you read the license and do you want to proceed? Type "yes": ' answer
if [[ "$answer" != "yes" ]]; then
  echo "Aborted. Nothing downloaded." >&2
  exit 1
fi

for tool in curl unzip sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "ERROR: $tool not found" >&2; exit 1; }
done

mkdir -p "$DEST"
archive="$DEST/dev.zip"

echo "Downloading to $archive ..."
# --fail          an HTML error page is an error, not a 3 KB "archive"
# --proto/-redir  https only, including across redirects: --location otherwise
#                 follows a redirect to plain http wherever the host points
# --max-filesize  a bounded download rather than one that fills the disk
curl --fail --location \
     --proto '=https' --proto-redir '=https' \
     --max-filesize 20000000000 \
     --progress-bar --output "$archive" "$BIRD_DEV_URL"

actual_sha="$(sha256sum "$archive" | awk '{print $1}')"
if [[ -z "$BIRD_DEV_SHA256" ]]; then
  cat >&2 <<EOF

No expected digest is pinned, so this archive cannot be verified.

  received: $actual_sha

If that matches what the BIRD project publishes, set BIRD_DEV_SHA256 at the top
of this script and re-run. The download is kept at $archive so you do not have
to fetch it twice. NOT extracting an unverified archive.
EOF
  exit 1
fi
if [[ "$actual_sha" != "$BIRD_DEV_SHA256" ]]; then
  echo "ERROR: digest mismatch — refusing to extract" >&2
  echo "  expected: $BIRD_DEV_SHA256" >&2
  echo "  received: $actual_sha" >&2
  exit 1
fi
echo "Digest verified."

echo "Extracting ..."
# -: refuses entries containing ".." so a crafted archive cannot write outside
# $DEST. Info-ZIP also restores symlinks, and a symlink entry followed by a
# file under it is the standard extraction escape.
unzip -q -o -: "$archive" -d "$DEST"

cat <<EOF

Done. Now verify what you actually got, rather than assuming:
  ls -R "$DEST" | head -40
  # question count:
  python3 -c "import json,glob;print(sum(len(json.load(open(f))) for f in glob.glob('$DEST/**/*dev.json', recursive=True)))"

Record the real counts and the license terms in PLAN.md section 6.1 once confirmed.
EOF
