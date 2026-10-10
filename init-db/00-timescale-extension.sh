#!/bin/sh
# The Timescale image's parent init hooks are replaced by this repository's
# mounted init-db directory. Restore the extension before application DDL runs.
set -eu

psql -v ON_ERROR_STOP=1 \
  --username "$POSTGRES_USER" \
  --dbname "$POSTGRES_DB" \
  -c 'CREATE EXTENSION IF NOT EXISTS timescaledb;'
