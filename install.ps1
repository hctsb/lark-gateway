# Installs or updates LarkGateway as a Windows service (NSSM).
# Run as administrator from the folder holding lark-gateway.exe, gateway.toml and .env:
#
#   powershell -ExecutionPolicy Bypass -File install.ps1
#
# It refuses to touch the service unless the config passes -check. It never
# reads or prints the secrets in .env; it only locks the file down.
$ErrorActionPreference = 'Stop'
$here    = Split-Path -Parent $MyInvocation.MyCommand.Path
$exe     = Join-Path $here 'lark-gateway.exe'
$nssm    = (Get-Command nssm -ErrorAction SilentlyContinue).Source
if (-not $nssm) { $nssm = 'C:\ProgramData\chocolatey\bin\nssm.exe' }   # choco install nssm
$service = 'LarkGateway'

foreach ($f in @($exe, (Join-Path $here 'gateway.toml'), (Join-Path $here '.env'), $nssm)) {
    if (-not (Test-Path $f)) { throw "Missing: $f" }
}

# 1. The config must pass before anything changes.
& $exe -config (Join-Path $here 'gateway.toml') -check
if ($LASTEXITCODE -ne 0) { throw 'gateway.toml or .env failed the check; nothing was changed.' }

# 2. .env: readable by SYSTEM and Administrators only.
icacls (Join-Path $here '.env') /inheritance:r /grant:r 'SYSTEM:F' 'Administrators:F' | Out-Null

# 3. Create the service, or stop it to update it.
$existing = Get-Service $service -ErrorAction SilentlyContinue
if ($existing) {
    if ($existing.Status -ne 'Stopped') { & $nssm stop $service | Out-Null }
} else {
    & $nssm install $service $exe | Out-Null
}
& $nssm set $service Application $exe | Out-Null
& $nssm set $service AppDirectory $here | Out-Null
& $nssm set $service DisplayName 'LarkGateway' | Out-Null
& $nssm set $service Description 'Holds the Lark WebSocket for each Lark app and runs the command each route asks for. Health: http://127.0.0.1:8791/' | Out-Null
& $nssm set $service Start SERVICE_AUTO_START | Out-Null
& $nssm set $service AppExit Default Restart | Out-Null
& $nssm set $service AppRestartDelay 10000 | Out-Null
& $nssm set $service AppStopMethodSkip 6 | Out-Null    # stop with Ctrl+C first, so the gateway shuts down cleanly
& $nssm set $service AppStopMethodConsole 15000 | Out-Null
New-Item -ItemType Directory -Force (Join-Path $here 'logs') | Out-Null
& $nssm set $service AppStdout (Join-Path $here 'logs\service-stdout.log') | Out-Null
& $nssm set $service AppStderr (Join-Path $here 'logs\service-stdout.log') | Out-Null
& $nssm set $service AppRotateFiles 1 | Out-Null
& $nssm set $service AppRotateBytes 10485760 | Out-Null

# 4. Start, then show health.
& $nssm start $service | Out-Null
Start-Sleep -Seconds 8
Get-Service $service | Format-Table Name, Status, StartType -AutoSize
try {
    (Invoke-WebRequest 'http://127.0.0.1:8791/' -UseBasicParsing -TimeoutSec 5).Content
} catch {
    Write-Warning "Health endpoint not answering yet: $_  See logs\gateway.log"
}
