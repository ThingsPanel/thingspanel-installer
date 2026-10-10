$ErrorActionPreference = 'Stop'
docker compose run --rm notification-db-init
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
