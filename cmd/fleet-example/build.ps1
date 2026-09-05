# build.ps1 — cross-compile the fleet example for the Compose images.
#
# The images are runtime-only (see Dockerfile.prebuilt): they contain a binary built
# here, on the host, from the working tree. Nothing is compiled inside a container, so
# what runs is unambiguously the code currently checked out rather than whatever the
# module proxy serves for this import path.
#
#   pwsh cmd/fleet-example/build.ps1
#   docker compose -f cmd/fleet-example/docker-compose.yml up
#
# CGO_ENABLED=0 because the runtime image is alpine, which has no glibc — a dynamically
# linked binary fails at exec with a message that does not mention libc.

$ErrorActionPreference = 'Stop'

# Repo root, two levels up from this script, so the script works from any directory.
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Push-Location $root

try {
    $out = Join-Path $root '.compose-bin'
    New-Item -ItemType Directory -Force -Path $out | Out-Null

    $targets = @{
        aggregator = './cmd/fleet-aggregator'
        gateway    = './cmd/fleet-example/gateway'
        orders     = './cmd/fleet-example/orders'
        auth       = './cmd/fleet-example/auth'
    }

    $env:CGO_ENABLED = '0'
    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'

    foreach ($name in $targets.Keys) {
        Write-Host "building $name from $($targets[$name])"
        & go build -trimpath -ldflags='-s -w' -o (Join-Path $out $name) $targets[$name]
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed for $name"
        }
    }

    Write-Host ''
    Write-Host 'built:'
    Get-ChildItem $out | ForEach-Object {
        Write-Host ("  {0,-12} {1,6:N1} MB" -f $_.Name, ($_.Length / 1MB))
    }
    Write-Host ''
    Write-Host 'next: docker compose -f cmd/fleet-example/docker-compose.yml up --build'
}
finally {
    Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}
