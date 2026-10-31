[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$ArchivePath,
    [Parameter(Mandatory = $true)]
    [string]$Version
)

$ErrorActionPreference = "Stop"
if ($PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1) {
    throw "Run this test with Windows PowerShell 5.1 (powershell.exe)."
}

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$archive = (Resolve-Path -LiteralPath $ArchivePath).Path

function Assert-CompatibleScript {
    param([string]$Path)
    $tokens = $null
    $parseErrors = $null
    [System.Management.Automation.Language.Parser]::ParseFile($Path, [ref]$tokens, [ref]$parseErrors) | Out-Null
    if ($parseErrors.Count -gt 0) {
        $messages = ($parseErrors | ForEach-Object { $_.Message }) -join "; "
        throw "Windows PowerShell 5.1 cannot parse ${Path}: $messages"
    }
    # These entry points are deliberately ASCII so ANSI and UTF-8 decoding agree.
    if (@([IO.File]::ReadAllBytes($Path) | Where-Object { $_ -gt 127 }).Count -gt 0) {
        throw "Windows entry point must be ASCII-only: $Path"
    }
}

Assert-CompatibleScript (Join-Path $repoRoot "scripts\windows\start.ps1")
Assert-CompatibleScript (Join-Path $repoRoot "scripts\windows\build.ps1")

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ("uart-sms-package-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $testRoot | Out-Null
Expand-Archive -LiteralPath $archive -DestinationPath $testRoot
$packageRoot = Join-Path $testRoot "uart_sms_forwarder-windows-amd64"
foreach ($name in @("uart_sms_forwarder.exe", "start.ps1", "config.yaml", "main.lua", "direct_forward.lua", "forward_config.lua", "README.md")) {
    if (-not (Test-Path -LiteralPath (Join-Path $packageRoot $name) -PathType Leaf)) {
        throw "Package is missing $name"
    }
}
Assert-CompatibleScript (Join-Path $packageRoot "start.ps1")

$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
try {
    $listener.Start()
    $port = $listener.LocalEndpoint.Port
}
finally {
    $listener.Stop()
}

# Keep the packaged default login, but isolate all writes and avoid USB discovery.
$sampleConfig = [IO.File]::ReadAllText((Join-Path $packageRoot "config.yaml"))
$adminMatch = [regex]::Match($sampleConfig, '(?m)^\s+admin:\s*"([^"\r\n]+)"')
if (-not $adminMatch.Success) { throw "Cannot locate the packaged default administrator." }
$adminHash = $adminMatch.Groups[1].Value
$configPath = Join-Path $packageRoot "smoke-config.yaml"
$configText = @"
database:
  enabled: true
  type: sqlite
  sqlite:
    path: "./runtime/database/app.db"
log:
  level: info
  filename: ./runtime/logs/smoke.log
server:
  addr: "127.0.0.1:$port"
App:
  JWT:
    Secret: "isolated-package-test-$([guid]::NewGuid().ToString('N'))"
    ExpiresHours: 1
  Users:
    admin: "$adminHash"
  Serial:
    AutoDiscover: false
    Port: "COM999999"
"@
[IO.File]::WriteAllText($configPath, $configText, [Text.UTF8Encoding]::new($true))
if (Test-Path -LiteralPath (Join-Path $packageRoot "runtime")) {
    throw "Runtime directories must be absent before startup."
}

$binaryPath = Join-Path $packageRoot "uart_sms_forwarder.exe"
$stdoutPath = Join-Path $testRoot "stdout.log"
$stderrPath = Join-Path $testRoot "stderr.log"
$process = $null
try {
    $process = Start-Process -FilePath $binaryPath -ArgumentList ('-config "{0}"' -f $configPath) -WorkingDirectory $packageRoot -WindowStyle Hidden -PassThru -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath
    $baseUrl = "http://127.0.0.1:$port"
    $ready = $false
    for ($attempt = 0; $attempt -lt 40; $attempt++) {
        if ($process.HasExited) { throw "Packaged server exited before becoming healthy. Logs: $testRoot" }
        try {
            $health = Invoke-RestMethod -Uri "$baseUrl/health" -TimeoutSec 1
            if ($health.status -eq "ok") { $ready = $true; break }
        }
        catch { }
        Start-Sleep -Milliseconds 250
    }
    if (-not $ready) { throw "Packaged server did not become healthy. Logs: $testRoot" }
    foreach ($directory in @("runtime\database", "runtime\logs")) {
        if (-not (Test-Path -LiteralPath (Join-Path $packageRoot $directory) -PathType Container)) {
            throw "Packaged server did not create configured directory: $directory"
        }
    }

    $login = Invoke-RestMethod -Method Post -Uri "$baseUrl/api/login" -ContentType "application/json" -Body '{"username":"admin","password":"admin123"}' -TimeoutSec 5
    if (-not $login.token) { throw "Default administrator login failed." }
    $versionResponse = Invoke-RestMethod -Uri "$baseUrl/api/version" -Headers @{Authorization = "Bearer " + $login.token} -TimeoutSec 5
    if ($versionResponse.version -ne $Version) {
        throw "Packaged version mismatch: expected $Version, got $($versionResponse.version)"
    }
    $index = Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/" -TimeoutSec 5
    $assetMatch = [regex]::Match($index.Content, 'src="(/assets/[^" ]+\.js)"')
    if (-not $assetMatch.Success) { throw "Embedded frontend does not reference a built JavaScript asset." }
    $asset = Invoke-WebRequest -UseBasicParsing -Uri ($baseUrl + $assetMatch.Groups[1].Value) -TimeoutSec 5
    if ($asset.StatusCode -ne 200 -or $asset.RawContentLength -lt 1) { throw "Embedded frontend asset request failed." }
    Write-Output "PASS Windows PowerShell 5.1 source/package parsing, health, login, version=$Version and embedded frontend."
}
finally {
    if ($null -ne $process -and -not $process.HasExited) {
        Stop-Process -Id $process.Id -Force
        $process.WaitForExit()
    }
    # Keep this uniquely named directory for CI diagnosis; never remove broad paths.
    Write-Output "Package test files and logs: $testRoot"
}
