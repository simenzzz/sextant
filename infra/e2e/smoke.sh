#!/bin/sh
# Smoke test run inside the compose network by docker-compose.test.yml.
#
# Retries rather than sleeping a fixed amount: a fixed sleep is either flaky on
# a slow runner or wasted time on a fast one.
set -eu

RETRIES=30
INTERVAL=2

wait_for() {
  name="$1"
  url="$2"
  i=1
  while [ "$i" -le "$RETRIES" ]; do
    if curl --silent --fail --max-time 3 "$url" >/dev/null 2>&1; then
      echo "ok: $name is up ($url)"
      return 0
    fi
    i=$((i + 1))
    sleep "$INTERVAL"
  done
  echo "FAIL: $name never became healthy at $url after $((RETRIES * INTERVAL))s" >&2
  return 1
}

wait_for "runtime"   "http://runtime:8080/healthz"
wait_for "retriever" "http://retriever:8000/healthz"

# Readiness is a separate claim from liveness, so assert it separately.
wait_for "retriever readiness" "http://retriever:8000/ready"

echo "smoke: all services healthy"

# A live question, end to end: HTTP in, the agent loop, the guard, the
# executor, and the SSE stream back out. Health checks prove the processes
# started; this proves the request path works.
#
# SEXTANT_PROVIDER is `fake` here, so no paid call is made and no credential is
# needed. The run therefore ends in a recorded failure rather than an answer —
# the fake reports no output, so there is no statement to run. What is asserted
# is that the run REACHES a terminal event: that is the whole path exercised,
# and a run that hangs or dies mid-stream fails this check.
echo "smoke: asking a question"

run_id=$(
  curl --silent --fail --max-time 10 \
    -X POST http://runtime:8080/v1/questions \
    -H 'Content-Type: application/json' \
    -d '{"schema":"question_request.v1","question":"how many orders are there?","database":"toy"}' \
  | sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p'
)

if [ -z "$run_id" ]; then
  echo "FAIL: POST /v1/questions returned no run_id" >&2
  exit 1
fi
echo "ok: run accepted ($run_id)"

events=$(curl --silent --fail --max-time 45 -N \
  -H 'Accept: text/event-stream' \
  "http://runtime:8080/v1/runs/$run_id/events" || true)

if [ -z "$events" ]; then
  echo "FAIL: the event stream produced nothing" >&2
  exit 1
fi

# Every run emits these, whatever the outcome.
for want in run_started retrieved generating; do
  if ! echo "$events" | grep -q "\"type\":\"$want\""; then
    echo "FAIL: no $want event in the stream" >&2
    echo "$events" >&2
    exit 1
  fi
done

# Exactly one of the terminal types must appear, or the client would be left
# watching a run that never finished.
if ! echo "$events" | grep -Eq '"type":"(answered|abstained|error|budget_exhausted|depth_exhausted|deadline_exceeded)"'; then
  echo "FAIL: the run reached no terminal event" >&2
  echo "$events" >&2
  exit 1
fi

# The cost ledger is emitted on every path, including a run that spent nothing.
if ! echo "$events" | grep -q 'cost_ledger.v1'; then
  echo "FAIL: no cost ledger frame" >&2
  exit 1
fi

echo "smoke: the question path works end to end"
