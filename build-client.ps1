param([string]$Go = 'go')

$ErrorActionPreference = 'Stop'
$originalGOOS, $originalGOARCH, $originalCGO = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED
Push-Location -LiteralPath $PSScriptRoot
try {
    New-Item -ItemType Directory -Force -Path dist | Out-Null
    & $Go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -manifest cmd/client/client.manifest -o cmd/client/client_windows_amd64.syso
    if ($LASTEXITCODE -ne 0) { throw 'Generating the Windows manifest failed.' }
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    & $Go build -trimpath -ldflags '-H windowsgui -s -w' -o dist/MultiFRP-windows-amd64.exe ./cmd/client
    if ($LASTEXITCODE -ne 0) { throw 'Building the Windows client failed.' }
} finally {
    $env:GOOS, $env:GOARCH, $env:CGO_ENABLED = $originalGOOS, $originalGOARCH, $originalCGO
    Pop-Location
}
