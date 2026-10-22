#!/usr/bin/env pwsh
# urnetwork-updater -- checks for URnetwork releases in the background
# GitHub: <https://github.com/urnetwork/connect>
#
# Started at sign-in by the urnetwork-update.lnk Startup shortcut that
# "urnet-tools auto-update on" writes. It wakes every hour and runs
# "urnet-tools update" once the interval has passed since the last successful
# check. Counting from a saved time, not one long sleep, means checks still
# happen on a PC that sleeps a lot or restarts often.
param(
    [String]$Frequency = "every-6h",
    [String]$InstalledPath = "",
    [Switch]$Once = $false
);

$Interval = switch ($Frequency) {
    "every-6h" { New-TimeSpan -Hours 6 }
    "every-day" { New-TimeSpan -Days 1 }
    "every-week" { New-TimeSpan -Days 7 }
    "every-month" { New-TimeSpan -Days 30 }
    default {
        Write-Error "Invalid frequency: $Frequency"
        exit 1
    }
}

if (-not $InstalledPath) {
    $InstalledPath = $PSScriptRoot
}

$ToolsPath = Join-Path $InstalledPath -ChildPath "urnet-tools.ps1"
$PIDFile = Join-Path $InstalledPath -ChildPath "urnetwork-updater.pid"
$LastCheckFile = Join-Path $InstalledPath -ChildPath "last-update-check"
$LogFile = Join-Path $InstalledPath -ChildPath "updater.log"
$AutoUpdateOffFile = Join-Path $InstalledPath -ChildPath "auto-update-off"
$PowerShellPath = Join-Path $PSHOME -ChildPath "powershell.exe"
if ($PSVersionTable.PSEdition -eq "Core") {
    $PowerShellPath = Join-Path $PSHOME -ChildPath "pwsh"
    if ($IsWindows) {
        $PowerShellPath = "$PowerShellPath.exe"
    }
}

# One updater per user, however often it is started.
$Mutex = New-Object System.Threading.Mutex($false, "Local\urnetwork-updater")
try {
    if (-not $Mutex.WaitOne(0)) {
        exit 0
    }
}
catch [System.Threading.AbandonedMutexException] {
    # The previous updater was killed; this one owns the mutex now.
}

Set-Content -LiteralPath $PIDFile -Value "$PID"

function Add-UpdaterLog {
    param([String]$Line)
    $Stamp = (Get-Date).ToString("u")
    Add-Content -LiteralPath $LogFile -Value "$Stamp $Line" -Encoding UTF8
}

function Get-LastCheck {
    try {
        $Text = "$(Get-Content -LiteralPath $LastCheckFile -Raw -ErrorAction Stop)".Trim()
        return [DateTime]::Parse($Text, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::RoundtripKind).ToUniversalTime()
    }
    catch {
        return [DateTime]::MinValue
    }
}

function Limit-Log {
    if ((Test-Path -LiteralPath $LogFile) -and (Get-Item -LiteralPath $LogFile).Length -gt 1MB) {
        $Tail = Get-Content -LiteralPath $LogFile -Tail 500
        Set-Content -LiteralPath $LogFile -Value $Tail -Encoding UTF8
    }
}

if (-not $Once) {
    # Give the sign-in a moment, and spread checks out a little.
    Start-Sleep -Seconds (Get-Random -Minimum 60 -Maximum 300)
}

while ($true) {
    # Turned off, or uninstalled, since this updater started.
    if ((Test-Path -LiteralPath $AutoUpdateOffFile) -or -not (Test-Path -LiteralPath $ToolsPath)) {
        Remove-Item -LiteralPath $PIDFile -Force -ErrorAction SilentlyContinue
        break
    }

    $Now = (Get-Date).ToUniversalTime()
    if (($Now - (Get-LastCheck)) -ge $Interval) {
        Limit-Log
        Add-UpdaterLog "checking for an update"
        $env:URNETWORK_REQUIRE_CHECKSUM = "1"
        & $PowerShellPath -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $ToolsPath update 2>&1 | ForEach-Object {
            Add-UpdaterLog "  $_"
        }
        if ($LASTEXITCODE -eq 0) {
            # Failures (offline, rate limited) are retried on the next hourly wake.
            Set-Content -LiteralPath $LastCheckFile -Value $Now.ToString("o", [Globalization.CultureInfo]::InvariantCulture)
        }
        else {
            Add-UpdaterLog "update check failed (exit code $LASTEXITCODE), retrying in an hour"
        }
    }

    if ($Once) {
        break
    }
    Start-Sleep -Seconds 3600
}
