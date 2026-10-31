$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

New-Item -ItemType Directory -Force -Path "data" | Out-Null
New-Item -ItemType Directory -Force -Path "logs" | Out-Null

if (-not (Test-Path -LiteralPath "config.yaml")) {
    throw "config.yaml was not found. Copy config.example.yaml and edit it before starting."
}

& ".\uart_sms_forwarder.exe" -config ".\config.yaml"
exit $LASTEXITCODE
