#!/usr/bin/env sh
set -eu
docker compose run --rm notification-migrate
docker compose run --rm -e NOTIFICATION_MIGRATE_MODE=apply notification-migrate
docker compose run --rm notification-migrate
