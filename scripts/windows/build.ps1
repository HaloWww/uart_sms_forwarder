[CmdletBinding()]
param(
    [ValidateSet("amd64", "arm64")]
    [string]$Architecture = "amd64",
    [string]$Version = "dev",
    [string]$OutputPath = "",
    [switch]$SkipWeb
)

$ErrorActionPreference = "Stop"
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$packageName = "uart_sms_forwarder-windows-$Architecture"
$stagingRoot = Join-Path $repoRoot ("dist\.windows-build-" + [guid]::NewGuid().ToString("N"))
$packageDirectory = Join-Path $stagingRoot $packageName
$archivePath = if ($OutputPath) {
    $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($OutputPath)
} else {
    Join-Path $repoRoot "dist\$packageName.zip"
}

Push-Location $repoRoot
try {
    if (-not $SkipWeb) {
        Push-Location "web"
        try {
            npm ci
            npm run build
        }
        finally {
            Pop-Location
        }
    }

    New-Item -ItemType Directory -Force -Path $packageDirectory | Out-Null
    New-Item -ItemType Directory -Force -Path (Join-Path $packageDirectory "data") | Out-Null
    New-Item -ItemType Directory -Force -Path (Join-Path $packageDirectory "logs") | Out-Null

    $previousCgo = $env:CGO_ENABLED
    $previousGoos = $env:GOOS
    $previousGoarch = $env:GOARCH
    try {
        $env:CGO_ENABLED = "0"
        $env:GOOS = "windows"
        $env:GOARCH = $Architecture
        $ldflags = "-s -w -X github.com/dushixiang/uart_sms_forwarder/internal/version.Version=$Version"
        go build "-ldflags=$ldflags" -o (Join-Path $packageDirectory "uart_sms_forwarder.exe") ./cmd/serv
    }
    finally {
        $env:CGO_ENABLED = $previousCgo
        $env:GOOS = $previousGoos
        $env:GOARCH = $previousGoarch
    }

    Copy-Item "config.example.yaml" (Join-Path $packageDirectory "config.yaml") -Force
    Copy-Item "main.lua" $packageDirectory -Force
    Copy-Item "scripts\windows\start.ps1" $packageDirectory -Force
    if (Test-Path $archivePath) {
        Remove-Item -LiteralPath $archivePath
    }
    Compress-Archive -Path $packageDirectory -DestinationPath $archivePath
    Write-Host "Windows package created: $archivePath"
}
finally {
    Pop-Location
    if (Test-Path -LiteralPath $stagingRoot) {
        Remove-Item -LiteralPath $stagingRoot -Recurse -Force
    }
}
