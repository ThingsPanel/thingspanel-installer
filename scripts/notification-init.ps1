$ErrorActionPreference = 'Stop'
docker compose run --rm --no-deps notification-setup
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
