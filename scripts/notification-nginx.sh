#!/usr/bin/env sh
set -eu
docker compose run --rm --no-deps notification-nginx-setup
docker compose exec -T frontend nginx -s reload
