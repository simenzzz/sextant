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
