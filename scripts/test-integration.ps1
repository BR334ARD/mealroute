$ErrorActionPreference = 'Stop'

$projectRoot = Split-Path -Parent $PSScriptRoot
$environmentFile = Join-Path $projectRoot '.env'
if (-not (Test-Path -LiteralPath $environmentFile)) {
    throw '.env is missing. Copy .env.example to .env before running integration tests.'
}

foreach ($line in Get-Content -LiteralPath $environmentFile) {
    $trimmed = $line.Trim()
    if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#')) {
        continue
    }
    $separator = $trimmed.IndexOf('=')
    if ($separator -le 0) {
        throw "Invalid .env entry: $line"
    }
    $name = $trimmed.Substring(0, $separator).Trim()
    $value = $trimmed.Substring($separator + 1)
    [Environment]::SetEnvironmentVariable($name, $value, 'Process')
}

Push-Location (Join-Path $projectRoot 'services/platform')
try {
    $env:GOCACHE = Join-Path (Get-Location) '.tmp-gocache'
    go test -count=1 -v ./internal/repository/postgres ./integration
    if ($LASTEXITCODE -ne 0) {
        throw "integration tests failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
