#!/bin/sh
# Creates the read-only role the agent executes generated SQL as.
#
# A .sh rather than a .sql because the entrypoint runs both, and only a script
# can take the password from the environment. The previous .sql version carried
# a hardcoded password and a comment saying deployments "must supply it from
# the environment" — with no mechanism to do so, which meant the placeholder
# was what would ship.
#
# The template it applies is named .sql.in, not .sql, on purpose: the Postgres
# entrypoint runs EVERY *.sql in this directory itself, and doing so here would
# execute the template a second time without the -v pw binding — which is a
# syntax error that fails the whole container init. Verified by hitting it.
#
# Fails closed: no password, no role, no database.
set -eu

: "${SEXTANT_READONLY_PASSWORD:?SEXTANT_READONLY_PASSWORD must be set}"

psql -v ON_ERROR_STOP=1 \
     -U "$POSTGRES_USER" \
     -d "$POSTGRES_DB" \
     -v pw="$SEXTANT_READONLY_PASSWORD" \
     -f /docker-entrypoint-initdb.d/readonly-role.sql.in
