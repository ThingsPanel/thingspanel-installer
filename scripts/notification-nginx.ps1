$ErrorActionPreference = 'Stop'
docker compose run --rm --no-deps notification-nginx-setup
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
docker compose exec -T frontend nginx -s reload
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
