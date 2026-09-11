$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

New-Item -ItemType Directory -Force -Path "data" | Out-Null
New-Item -ItemType Directory -Force -Path "logs" | Out-Null

if (-not (Test-Path -LiteralPath "config.yaml")) {
    throw "找不到 config.yaml，请先复制并修改 config.example.yaml。"
}

& ".\uart_sms_forwarder.exe" -config ".\config.yaml"
