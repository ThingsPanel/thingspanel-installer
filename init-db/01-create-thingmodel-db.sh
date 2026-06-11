#!/usr/bin/env bash
# Creates the device_metadata database for the thingmodel-api (Encore) service.
# This script runs automatically on fresh PostgreSQL data volumes only.
# For existing deployments run: psql -U postgres -c 'CREATE DATABASE device_metadata;'
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    SELECT 'CREATE DATABASE device_metadata OWNER $POSTGRES_USER'
    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'device_metadata') \gexec
EOSQL
