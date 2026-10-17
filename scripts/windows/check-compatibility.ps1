# Build/vet and verify the real Windows herdr API + terminal bridge in isolation.
# Example: .\scripts\windows\check-compatibility.ps1 -HerdrPath C:\path\herdr.exe
param([string]$HerdrPath)
$ErrorActionPreference = 'Stop'
$repoDirectory = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$originalPath = $env:Path
$originalCGO = $env:CGO_ENABLED
Push-Location $repoDirectory
try {
    if ($HerdrPath) {
        $resolvedHerdr = (Resolve-Path -LiteralPath $HerdrPath).Path
        $env:Path = (Split-Path -Parent $resolvedHerdr) + ';' + $env:Path
    }
    $herdrCommand = Get-Command herdr -ErrorAction Stop
    & $herdrCommand.Source --version
    if ($LASTEXITCODE -ne 0) { throw 'herdr cannot start' }
    $env:CGO_ENABLED = '0'
    & go build ./...
    if ($LASTEXITCODE -ne 0) { throw 'Windows build failed' }
    & go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Windows vet failed' }
    $testBinary = Join-Path $repoDirectory 'bin\herdrapi-windows.test.exe'
    New-Item -ItemType Directory -Path (Split-Path -Parent $testBinary) -Force | Out-Null
    & go test -c -o $testBinary ./internal/herdrapi
    if ($LASTEXITCODE -ne 0) { throw 'Windows test build failed' }
    & $testBinary '-test.run=^TestWindows' '-test.v' '-test.timeout=90s'
    if ($LASTEXITCODE -ne 0) { throw 'Windows compatibility check failed; production binaries were not changed' }
} finally {
    $env:Path = $originalPath
    $env:CGO_ENABLED = $originalCGO
    Pop-Location
}
