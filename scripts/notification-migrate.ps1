$ErrorActionPreference = 'Stop'
docker compose run --rm notification-migrate
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
docker compose run --rm -e NOTIFICATION_MIGRATE_MODE=apply notification-migrate
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
docker compose run --rm notification-migrate
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
