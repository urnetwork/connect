#!/usr/bin/env pwsh
# Credits: Ar Rakin, Ryan Mello
# urnet-tools -- URnetwork manager script
# GitHub: <https://github.com/urnetwork/connect>
<#
.SYNOPSIS
    URnetwork manager and toolkit.

.DESCRIPTION
    This script helps you manage your URnetwork installation.

    urnet-tools start | stop | restart | status | logs
    urnet-tools update
    urnet-tools auto-update [on | off] [-Interval 6h|day|week|month]
    urnet-tools auto-start [on | off]
    urnet-tools version
    urnet-tools reinstall [-t TAG]
    urnet-tools uninstall [-RemoveIdentity [-Yes]]

    From cmd.exe, or when PowerShell's execution policy blocks scripts, run
    urnet-tools.cmd with the same arguments.

.PARAMETER Command
    The subcommand to execute. The first positional argument also
    corresponds to this.
    Subcommands:
    * start - Start the provider in the background
    * stop - Stop the provider
    * restart - Stop and start the provider
    * status - Show whether the provider runs, its version and settings
      (exit code 0 when running, 3 when not)
    * logs - Follow the provider's log (Ctrl-C to quit)
    * update - Update URnetwork if a newer release is out
    * version - Show the installed version and whether an update is out
    * auto-update - Show, or turn on or off, automatic updates
    * auto-start - Show, or turn on or off, starting at sign-in
    * reinstall - Reinstall the installed (or -Version) release
    * uninstall - Uninstall URnetwork; your identity is kept unless -RemoveIdentity
    * help - Show this help
    Older names still work: auto-update-enable, auto-update-disable,
    auto-update-freq, auto-start-enable, auto-start-disable.

.PARAMETER Value
    on, off or status, for auto-update and auto-start.

.PARAMETER Interval
    How often automatic updates check for a release: 6h (default), day, week
    or month (daily, weekly and monthly work too). Given alone, it changes the
    interval and keeps automatic updates on or off.

.PARAMETER Version
    For reinstall (also -t or -Tag): the release tag to install instead of the
    installed one.

.PARAMETER RemoveIdentity
    For uninstall (also -RemoveData): also delete your identity and sign-in
    (%USERPROFILE%\.urnetwork). It asks you to type "delete" first. Without
    it they are kept, so a later install is the same miner.

.PARAMETER Yes
    For uninstall -RemoveIdentity: don't ask. Without a person to ask and
    without -Yes, nothing is removed.

.PARAMETER InstalledPath
    The path where URnetwork provider was installed. Defaults to the folder of this script.

.PARAMETER Help
    Show this help and exit.

.EXAMPLE
    urnet-tools status

.EXAMPLE
    urnet-tools auto-update off

.EXAMPLE
    urnet-tools uninstall -RemoveIdentity

.LINK
    https://ur.xyz/docs/miner
#>

param(
    [Parameter(Position = 0)]
    [ValidateSet("uninstall", "update", "start", "stop", "restart", "status", "logs", "version", "reinstall", "help", "auto-update", "auto-start", "auto-update-enable", "auto-update-disable", "auto-update-freq", "auto-start-enable", "auto-start-disable")]
    [String]$Command,
    [Parameter(Position = 1)]
    [String]$Value = "",
    [String]$Interval = "",
    [Alias("t", "Tag")]
    [String]$Version = "",
    [Alias("RemoveData")]
    [Switch]$RemoveIdentity = $false,
    [Switch]$Yes = $false,
    [Switch]$Help = $false,
    [String]$InstalledPath = "",
    [Parameter(ValueFromRemainingArguments = $true)]
    [String[]]$Rest = @()
);

$ErrorActionPreference = "Stop"

# Linux-style options work too (--remove-identity, --yes, --tag X,
# --interval=X), so the same instructions fit every platform.
$Words = @(@($Value) + @($Rest) | Where-Object { $_ -ne "" -and $null -ne $_ })
$Value = ""
for ($Index = 0; $Index -lt $Words.Count; $Index++) {
    $Word = $Words[$Index]
    $Next = ""
    if ($Index + 1 -lt $Words.Count) {
        $Next = $Words[$Index + 1]
    }
    switch -regex ($Word) {
        '^(on|off|status)$' { $Value = $Word; break }
        '^(6h|every-6h|day|daily|every-day|week|weekly|every-week|month|monthly|every-month)$' { $Interval = $Word; break }
        '^--(remove-identity|purge)$' { $RemoveIdentity = $true; break }
        '^--yes$' { $Yes = $true; break }
        '^--(tag|interval)=(.+)$' {
            if ($Matches[1] -eq "tag") { $Version = $Matches[2] } else { $Interval = $Matches[2] }
            break
        }
        '^--(tag|interval)$' {
            if (-not $Next) {
                Write-Host "$Word needs a value"
                exit 1
            }
            if ($Word -eq "--tag") { $Version = $Next } else { $Interval = $Next }
            $Index++
            break
        }
        default {
            Write-Host "Unknown option '$Word'. See: urnet-tools help"
            exit 1
        }
    }
}

if ($Help -or $Command -eq "help") {
    Get-Help $PSCommandPath -Detailed
    exit 0
}

if (-not $Command) {
    Write-Host "Please give a command, for example: urnet-tools status"
    Write-Host "All commands: urnet-tools help"
    exit 1
}

$OnWindows = ($PSVersionTable.PSVersion.Major -lt 6 -or $IsWindows)

if ($PSVersionTable.PSVersion.Major -lt 6) {
    # Windows PowerShell 5.1 may still default to TLS 1.0, which GitHub refuses.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}

if (-not $InstalledPath) {
    $InstalledPath = $PSScriptRoot
}
$InstalledPath = [IO.Path]::GetFullPath($InstalledPath).TrimEnd('\', '/')

$BinarySuffix = ""
if ($OnWindows) {
    $BinarySuffix = ".exe"
}

$DefaultScriptsBaseUrl = "https://raw.githubusercontent.com/urnetwork/connect/refs/heads/main/scripts"
$GithubURLBase = $env:URNETWORK_RELEASE_API_URL
if (-not $GithubURLBase) {
    $GithubURLBase = "https://api.github.com/repos/urnetwork/build"
}
$GithubURLBase = $GithubURLBase.TrimEnd('/')

$BinaryPath = Join-Path $InstalledPath -ChildPath "urnetwork$BinarySuffix"
$ToolsPath = Join-Path $InstalledPath -ChildPath "urnet-tools.ps1"
$UpdaterPath = Join-Path $InstalledPath -ChildPath "urnetwork-updater.ps1"
$VersionPath = Join-Path $InstalledPath -ChildPath "version"
$InstallDatePath = Join-Path $InstalledPath -ChildPath "date"
$ScriptsUrlPath = Join-Path $InstalledPath -ChildPath "scripts-url"
$AutoUpdateOffPath = Join-Path $InstalledPath -ChildPath "auto-update-off"
$IntervalPath = Join-Path $InstalledPath -ChildPath "auto-update-interval"
$LastCheckPath = Join-Path $InstalledPath -ChildPath "last-update-check"
$UpdaterLogPath = Join-Path $InstalledPath -ChildPath "updater.log"
$UpdaterPIDPath = Join-Path $InstalledPath -ChildPath "urnetwork-updater.pid"
$ProviderLogPath = Join-Path $InstalledPath -ChildPath "provider.log"

# The background provider starts at sign-in with the saved (user or
# machine) environment, so that decides its state folder. A value set only in
# this window is dropped, or a later start would use a different identity.
if ($OnWindows) {
    $SavedStateDir = [Environment]::GetEnvironmentVariable("URNETWORK_STATE_DIR", [EnvironmentVariableTarget]::User)
    if (-not $SavedStateDir) {
        $SavedStateDir = [Environment]::GetEnvironmentVariable("URNETWORK_STATE_DIR", [EnvironmentVariableTarget]::Machine)
    }
    if ("$env:URNETWORK_STATE_DIR" -ne "$SavedStateDir") {
        Write-Host "Note: ignoring URNETWORK_STATE_DIR set in this window only; the background miner uses $(if ($SavedStateDir) { $SavedStateDir } else { '%USERPROFILE%\.urnetwork' })"
        $env:URNETWORK_STATE_DIR = $SavedStateDir
    }
}

$StateDir = $env:URNETWORK_STATE_DIR
if (-not $StateDir) {
    $StateDir = Join-Path $HOME -ChildPath ".urnetwork"
}
elseif (-not [IO.Path]::IsPathRooted($StateDir)) {
    # The miner runs in the install folder and resolves it from there.
    $StateDir = Join-Path $InstalledPath -ChildPath $StateDir
}

# The miner's own "provider update" looks for these two names, so keep them.
$AutoStartShortcut = ""
$AutoUpdateShortcut = ""
if ($OnWindows) {
    $StartupPath = [Environment]::GetFolderPath("Startup")
    $AutoStartShortcut = Join-Path $StartupPath -ChildPath "urnetwork.lnk"
    $AutoUpdateShortcut = Join-Path $StartupPath -ChildPath "urnetwork-update.lnk"
}

# Get-PowerShellPath is the PowerShell to run child scripts with: this one's
# edition, never the host program.
function Get-PowerShellPath {
    if ($PSVersionTable.PSEdition -eq "Core") {
        if ($OnWindows) {
            return Join-Path $PSHOME -ChildPath "pwsh.exe"
        }
        return Join-Path $PSHOME -ChildPath "pwsh"
    }
    return Join-Path $PSHOME -ChildPath "powershell.exe"
}

function Read-TextFile {
    param([String]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        return ""
    }
    return "$(Get-Content -LiteralPath $Path -Raw)".Trim()
}

function Get-InstalledTag {
    return Read-TextFile $VersionPath
}

function Get-ScriptsBaseUrl {
    $Url = Read-TextFile $ScriptsUrlPath
    if (-not $Url) {
        $Url = $DefaultScriptsBaseUrl
    }
    return $Url.TrimEnd('/')
}

# ConvertTo-Interval accepts the names the Linux urnet-tools uses too.
function ConvertTo-Interval {
    param([String]$Name)
    switch ("$Name".Trim().ToLower()) {
        { $_ -in @("6h", "every-6h") } { return "6h" }
        { $_ -in @("day", "daily", "every-day") } { return "day" }
        { $_ -in @("week", "weekly", "every-week") } { return "week" }
        { $_ -in @("month", "monthly", "every-month") } { return "month" }
    }
    return ""
}

function Get-Interval {
    $Saved = ConvertTo-Interval (Read-TextFile $IntervalPath)
    if ($Saved) {
        return $Saved
    }
    return "6h"
}

function Get-LatestTag {
    $Headers = @{ "Accept" = "application/vnd.github+json"; "User-Agent" = "urnet-tools" }
    try {
        $ReleaseInfo = Invoke-RestMethod -Uri "$GithubURLBase/releases/latest" -Headers $Headers -UseBasicParsing
    }
    catch {
        throw "Couldn't check for a new release ($($_.Exception.Message)). Check your internet connection."
    }
    if (-not $ReleaseInfo.tag_name) {
        throw "The release information from $GithubURLBase has no version tag"
    }
    return "$($ReleaseInfo.tag_name)"
}

# Get-ProviderProcess returns the ids of providers running "provide" from this
# install folder. Other runs of the same exe ("auth", or the miner's own
# "provider update", which may be what called urnet-tools) are left alone.
function Get-ProviderProcess {
    $Prefix = $InstalledPath + [IO.Path]::DirectorySeparatorChar
    $Provide = '(^|\s)"?provide"?(\s|$)'
    if ($OnWindows) {
        try {
            return @(Get-CimInstance -ClassName Win32_Process -Filter "Name = 'urnetwork.exe'" -ErrorAction Stop | Where-Object {
                $_.ExecutablePath -and $_.ExecutablePath.StartsWith($Prefix, [StringComparison]::OrdinalIgnoreCase) -and
                ($_.CommandLine -replace '^("[^"]*"|\S+)', '') -match $Provide
            } | ForEach-Object { [int]$_.ProcessId })
        }
        catch {
            # Without WMI the arguments can't be seen; count every urnetwork.exe
            # from this folder rather than none, so nothing starts twice.
            return @(Get-Process -Name "urnetwork" -ErrorAction SilentlyContinue | Where-Object {
                $_.Path -and $_.Path.StartsWith($Prefix, [StringComparison]::OrdinalIgnoreCase)
            } | ForEach-Object { $_.Id })
        }
    }
    return @(Get-Process -Name "urnetwork" -ErrorAction SilentlyContinue | Where-Object {
        $_.Path -and $_.Path.StartsWith($Prefix, [StringComparison]::OrdinalIgnoreCase) -and
        ($_.CommandLine -replace '^\S+', '') -match $Provide
    } | ForEach-Object { $_.Id })
}

function Get-UpdaterProcess {
    if (-not $OnWindows) {
        return @()
    }
    $Found = @()
    try {
        $Found = @(Get-CimInstance -ClassName Win32_Process -Filter "Name = 'powershell.exe' OR Name = 'pwsh.exe'" -ErrorAction Stop | Where-Object {
            $_.CommandLine -and $_.CommandLine -like "*urnetwork-updater.ps1*" -and $_.ProcessId -ne $PID
        })
    }
    catch {}
    return $Found
}

function Test-SignedIn {
    return (Test-Path -LiteralPath (Join-Path $StateDir -ChildPath "jwt"))
}

# Test-Registered is true once the miner has registered this device: like
# the miner, any .provider*.jwt counts (proxy setups name theirs
# .provider-<hash>.jwt).
function Test-Registered {
    return @(Get-ChildItem -LiteralPath $StateDir -Force -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ".provider*.jwt" }).Count -gt 0
}

function Test-StateFile {
    param([String]$Name)
    return (Test-Path -LiteralPath (Join-Path $StateDir -ChildPath $Name))
}

# Get-IdentityProblem names states the miner refuses to start from, which
# no registration or restart fixes.
function Get-IdentityProblem {
    if (Test-StateFile ".provider.jwt.rejected") {
        return "This device was removed from your URnetwork account, so the network won't accept it again. To mine from this PC as a new device, rename the folder $StateDir (keep it, don't delete it), sign in with 'urnetwork auth' and start again."
    }
    if ((Test-StateFile ".provider.jwt") -and -not (Test-StateFile ".provider.key")) {
        return "This device's identity is from an older miner and needs a one-time recovery step before this version can use it. See https://ur.xyz/docs/miner (Troubleshooting)."
    }
    return ""
}

# Get-StartFailureHint explains the usual reasons a provider stops at once,
# from what this start added to its log.
function Get-StartFailureHint {
    param([String]$Marker)
    $Text = ""
    foreach ($Log in @($ProviderLogPath, "$ProviderLogPath.err")) {
        if (Test-Path -LiteralPath $Log) {
            $Text += "$(Get-Content -LiteralPath $Log -Raw)`n"
        }
    }
    $At = $Text.LastIndexOf($Marker)
    if ($At -ge 0) {
        $Text = $Text.Substring($At)
    }

    if ($Text -match "startup_recovery_required") {
        # The miner logs only the event, not why. A device that never got a
        # key here means registration itself failed.
        if (-not (Test-Registered) -and -not (Test-StateFile ".provider.key")) {
            if ($OnWindows) {
                return "This URnetwork release can't register a new device on Windows yet. Windows support is on its way; until then this PC can't mine with the official release."
            }
            return "Registering this device failed. See $ProviderLogPath."
        }
        return "This device's miner identity in $StateDir is incomplete (files missing or changed). Restore your backup of that folder."
    }
    if ($Text -match "client_revoked") {
        return (Get-IdentityProblem)
    }
    if ($Text -match "network_sign_in_rejected") {
        return "The network didn't accept this device's sign-in. Sign in again with 'urnetwork auth', then 'urnet-tools start'."
    }
    return ""
}

function Start-Provider {
    if (-not (Test-Path -LiteralPath $BinaryPath)) {
        throw "$BinaryPath is missing. Reinstall URnetwork."
    }
    if ((Get-ProviderProcess).Count -gt 0) {
        Write-Host "URnetwork is already running"
        return
    }
    if (-not (Test-SignedIn)) {
        throw "You're not signed in yet. Sign in with 'urnetwork auth' (auth code from https://ur.io), then start again."
    }
    $Problem = Get-IdentityProblem
    if ($Problem) {
        throw $Problem
    }

    # Keep the last log beside the current one.
    if ((Test-Path -LiteralPath $ProviderLogPath) -and (Get-Item -LiteralPath $ProviderLogPath).Length -gt 10MB) {
        Move-Item -LiteralPath $ProviderLogPath -Destination "$ProviderLogPath.1" -Force
    }

    # A new device is registered on its first start, which the miner only
    # does when asked. It never replaces an identity it already has, and once
    # .provider.jwt exists later starts don't ask again.
    $ProvideArguments = "provide"
    $Registering = -not (Test-Registered)
    if ($Registering) {
        # Older miners don't have the flag and would refuse to start with it.
        $ErrorActionPreference = "Continue"
        $ProvideHelp = "$(& $BinaryPath provide --help 2>&1)"
        $ErrorActionPreference = "Stop"
        $Registering = $ProvideHelp -match "allow-client-registration"
    }
    if ($Registering) {
        Write-Host "First start on this device: registering it with the network"
        $ProvideArguments = "provide --allow-client-registration"
    }

    # Marks where this start's output begins in the appended log.
    $Marker = "=== urnet-tools start $((Get-Date).ToString('u')) ==="
    Add-Content -LiteralPath $ProviderLogPath -Value $Marker

    if ($OnWindows) {
        # cmd.exe writes the provider's output to its log; the window is hidden.
        # cmd strips the outer quotes of /c "..." and keeps the inner ones.
        $Line = "/d /c `"`"$BinaryPath`" $ProvideArguments >> `"$ProviderLogPath`" 2>&1`""
        Start-Process -FilePath $env:ComSpec -ArgumentList $Line -WorkingDirectory $InstalledPath -WindowStyle Hidden
    }
    else {
        Start-Process -FilePath $BinaryPath -ArgumentList $ProvideArguments -WorkingDirectory $InstalledPath -RedirectStandardOutput $ProviderLogPath -RedirectStandardError "$ProviderLogPath.err"
    }

    for ($Wait = 0; $Wait -lt 10 -and (Get-ProviderProcess).Count -eq 0; $Wait++) {
        Start-Sleep -Milliseconds 500
    }
    Start-Sleep -Seconds 2
    if ($Registering) {
        # Registration may be refused a little later; wait for its outcome.
        # A refused sign-in won't fix itself, so it stops the wait at once.
        for ($Wait = 0; $Wait -lt 30 -and -not (Test-Registered) -and (Get-ProviderProcess).Count -gt 0; $Wait++) {
            Start-Sleep -Seconds 1
            $Recent = "$(Get-Content -LiteralPath $ProviderLogPath -Raw -ErrorAction SilentlyContinue)"
            $At = $Recent.LastIndexOf($Marker)
            if ($At -gt 0) {
                $Recent = $Recent.Substring($At)
            }
            if ($Recent -match "network_sign_in_rejected|client_revoked") {
                Get-ProviderProcess | ForEach-Object { Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue }
                $Hint = Get-StartFailureHint -Marker $Marker
                if (-not $Hint) {
                    $Hint = "The network didn't accept this device's sign-in. Sign in again with 'urnetwork auth', then 'urnet-tools start'."
                }
                throw "Registering this device was refused. $Hint"
            }
        }
        if ((Test-Registered)) {
            Write-Host "This device is registered"
        }
        elseif ((Get-ProviderProcess).Count -gt 0) {
            Write-Host "Still registering (the network may be slow); 'urnet-tools status' shows when it's done"
        }
    }
    if ((Get-ProviderProcess).Count -gt 0) {
        Write-Host "URnetwork is running in the background. Its log: $ProviderLogPath"
    }
    else {
        $Hint = Get-StartFailureHint -Marker $Marker
        if (-not $Hint) {
            $Hint = "See $ProviderLogPath, or run 'urnetwork provide' in a window."
        }
        throw "URnetwork stopped right after starting. $Hint"
    }
}

function Stop-Provider {
    $Running = Get-ProviderProcess
    if ($Running.Count -eq 0) {
        Write-Host "URnetwork isn't running"
        return
    }
    $Running | ForEach-Object { Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue }
    Write-Host "Stopped URnetwork"
}

function Stop-Updater {
    $CimWorks = $true
    try {
        $null = Get-CimInstance -ClassName Win32_Process -Filter "ProcessId = $PID" -ErrorAction Stop
    }
    catch {
        $CimWorks = $false
    }
    foreach ($Process in Get-UpdaterProcess) {
        Stop-Process -Id $Process.ProcessId -Force -ErrorAction SilentlyContinue
    }
    # Without WMI, the PID file names the updater. The process must have
    # started before the file was written, or the id now belongs to another
    # PowerShell.
    $UpdaterPID = 0
    if (-not $CimWorks -and [int]::TryParse((Read-TextFile $UpdaterPIDPath), [ref]$UpdaterPID)) {
        $Process = Get-Process -Id $UpdaterPID -ErrorAction SilentlyContinue
        $Written = (Get-Item -LiteralPath $UpdaterPIDPath).LastWriteTime
        if ($Process -and ($Process.ProcessName -eq "powershell" -or $Process.ProcessName -eq "pwsh") -and $Process.StartTime -le $Written) {
            Stop-Process -Id $UpdaterPID -Force -ErrorAction SilentlyContinue
        }
    }
    Remove-Item -LiteralPath $UpdaterPIDPath -Force -ErrorAction SilentlyContinue
}

function Assert-Windows {
    param([String]$What)
    if (-not $OnWindows) {
        throw "$What is only available on Windows"
    }
}

# New-Shortcut writes a minimized PowerShell shortcut that runs a script from
# the install folder. -ExecutionPolicy Bypass is needed because Windows'
# default policy (Restricted) stops "-File" scripts from running.
function New-Shortcut {
    param(
        [String]$Path,
        [String]$Arguments
    )
    $WshShell = New-Object -ComObject WScript.Shell
    $Shortcut = $WshShell.CreateShortcut($Path)
    $Shortcut.TargetPath = Join-Path $env:SystemRoot -ChildPath "System32\WindowsPowerShell\v1.0\powershell.exe"
    $Shortcut.Arguments = "-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass $Arguments"
    $Shortcut.WorkingDirectory = $InstalledPath
    $Shortcut.WindowStyle = 7
    $Shortcut.Description = "URnetwork"
    $Shortcut.Save()
}

function Enable-AutoUpdate {
    param([String]$NewInterval = "")
    Assert-Windows "Automatic update"

    $Frequency = Get-Interval
    if ($NewInterval) {
        $Frequency = ConvertTo-Interval $NewInterval
        if (-not $Frequency) {
            throw "Unknown interval '$NewInterval'. Use 6h, day, week or month."
        }
    }
    Set-Content -LiteralPath $IntervalPath -Value $Frequency
    Remove-Item -LiteralPath $AutoUpdateOffPath -Force -ErrorAction SilentlyContinue

    # Always (re)written, so a shortcut from an older installer gets fixed.
    New-Shortcut -Path $AutoUpdateShortcut -Arguments "-File `"$UpdaterPath`" -InstalledPath `"$InstalledPath`" -Frequency every-$Frequency"

    if ((Get-UpdaterProcess).Count -eq 0) {
        Start-Process -FilePath $AutoUpdateShortcut -WindowStyle Hidden
    }
    elseif ($NewInterval) {
        # The running updater has the old interval; start it again.
        Stop-Updater
        Start-Process -FilePath $AutoUpdateShortcut -WindowStyle Hidden
    }
    Write-Host "Automatic updates are on (checks every $Frequency)"
}

function Disable-AutoUpdate {
    Assert-Windows "Automatic update"
    Set-Content -LiteralPath $AutoUpdateOffPath -Value "Turned off with urnet-tools on $((Get-Date).ToString('u'))"
    Stop-Updater
    Remove-Item -LiteralPath $AutoUpdateShortcut -Force -ErrorAction SilentlyContinue
    Write-Host "Automatic updates are off. 'urnet-tools update' still updates by hand."
}

function Show-AutoUpdate {
    if (-not $OnWindows) {
        Write-Host "Automatic updates: not available on this platform"
        return
    }
    if (Test-Path -LiteralPath $AutoUpdateShortcut) {
        $Last = Read-TextFile $LastCheckPath
        if (-not $Last) {
            $Last = "not yet"
        }
        Write-Host "Automatic updates: on (every $(Get-Interval), last check: $Last)"
    }
    else {
        Write-Host "Automatic updates: off"
    }
}

function Enable-AutoStart {
    Assert-Windows "Starting at sign-in"
    # Always (re)written, so a shortcut from an older installer gets fixed.
    New-Shortcut -Path $AutoStartShortcut -Arguments "-File `"$ToolsPath`" start"
    Write-Host "URnetwork starts in the background when you sign in to Windows"
}

function Disable-AutoStart {
    Assert-Windows "Starting at sign-in"
    Remove-Item -LiteralPath $AutoStartShortcut -Force -ErrorAction SilentlyContinue
    Write-Host "URnetwork no longer starts when you sign in. It keeps running until you stop it or sign out."
}

function Show-AutoStart {
    if (-not $OnWindows) {
        Write-Host "Start at sign-in: not available on this platform"
        return
    }
    if (Test-Path -LiteralPath $AutoStartShortcut) {
        Write-Host "Start at sign-in: on"
    }
    else {
        Write-Host "Start at sign-in: off"
    }
}

# Path entries are compared without case, a trailing backslash or unexpanded
# variables, the way Windows itself resolves them.
function Test-SamePathEntry {
    param(
        [String]$A,
        [String]$B
    )
    $Left = [Environment]::ExpandEnvironmentVariables($A.Trim()).TrimEnd('\', '/')
    $Right = [Environment]::ExpandEnvironmentVariables($B.Trim()).TrimEnd('\', '/')
    return ($Left -ieq $Right)
}

function Remove-PathEntry {
    param(
        [String]$PathValue,
        [String]$Entry
    )
    $Kept = @("$PathValue".Split(";") | Where-Object { $_.Trim() -ne "" -and -not (Test-SamePathEntry $_ $Entry) })
    return $Kept -join ";"
}

# The user PATH is changed in the registry so entries like %USERPROFILE%\bin
# stay as they are (see the installer).
function Remove-FromUserPath {
    $Key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment", $true)
    if (-not $Key) {
        return
    }
    try {
        $PathValue = [String]$Key.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $NewPathValue = Remove-PathEntry -PathValue $PathValue -Entry $InstalledPath
        if ($NewPathValue -ne $PathValue) {
            Write-Host "Removing $InstalledPath from your PATH"
            $Key.SetValue("Path", $NewPathValue, [Microsoft.Win32.RegistryValueKind]::ExpandString)
        }
    }
    finally {
        $Key.Close()
    }
    # Removing a variable this way also tells Windows the environment changed.
    [Environment]::SetEnvironmentVariable("URNETWORK_INSTALL_DIR", $null, [EnvironmentVariableTarget]::User)
}

# Confirm-RemoveIdentity asks before deleting the identity, which can't be
# undone. With nobody to ask and no -Yes, it refuses.
function Confirm-RemoveIdentity {
    if ($Yes -or -not (Test-Path -LiteralPath $StateDir)) {
        return
    }
    if ([Console]::IsInputRedirected -or -not [Environment]::UserInteractive) {
        throw "-RemoveIdentity deletes $StateDir for good. Add -Yes to do that without a question. Nothing was removed."
    }
    Write-Host "This deletes your URnetwork identity and sign-in in $StateDir. It can't be undone."
    $Answer = Read-Host "Type delete to confirm"
    if ("$Answer".Trim() -ne "delete") {
        throw "Not confirmed. Nothing was removed."
    }
}

function Invoke-Uninstall {
    if ($RemoveIdentity) {
        Confirm-RemoveIdentity
    }
    Write-Host "Uninstalling URnetwork from $InstalledPath"

    if ($OnWindows) {
        Remove-Item -LiteralPath $AutoStartShortcut -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $AutoUpdateShortcut -Force -ErrorAction SilentlyContinue
        Stop-Updater
    }

    $Running = Get-ProviderProcess
    if ($Running.Count -gt 0) {
        Write-Host "Stopping URnetwork"
        $Running | ForEach-Object { Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue }
        Start-Sleep -Seconds 1
    }

    $Files = @(
        $BinaryPath, $ToolsPath, $UpdaterPath, (Join-Path $InstalledPath -ChildPath "urnet-tools.cmd"),
        $VersionPath, $InstallDatePath, $ScriptsUrlPath, $AutoUpdateOffPath, $IntervalPath,
        $LastCheckPath, $UpdaterLogPath, $UpdaterPIDPath,
        $ProviderLogPath, "$ProviderLogPath.1", "$ProviderLogPath.err"
    )
    foreach ($File in $Files) {
        Remove-Item -LiteralPath $File -Force -ErrorAction SilentlyContinue
    }
    Get-ChildItem -LiteralPath $InstalledPath -Filter "urnetwork$BinarySuffix.old*" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue

    if (Test-Path -LiteralPath $BinaryPath) {
        throw "Couldn't remove $BinaryPath. Close any window running URnetwork and try again."
    }

    # Only folders that are now empty are removed.
    if (-not (Get-ChildItem -LiteralPath $InstalledPath -Force -ErrorAction SilentlyContinue)) {
        Remove-Item -LiteralPath $InstalledPath -Force -ErrorAction SilentlyContinue
        $Parent = Split-Path -Parent $InstalledPath
        if ((Split-Path -Leaf $Parent) -eq "urnetwork" -and -not (Get-ChildItem -LiteralPath $Parent -Force -ErrorAction SilentlyContinue)) {
            Remove-Item -LiteralPath $Parent -Force -ErrorAction SilentlyContinue
        }
    }

    if ($OnWindows) {
        Remove-FromUserPath
    }

    if ($RemoveIdentity) {
        if (Test-Path -LiteralPath $StateDir) {
            Write-Host "Deleting your identity and sign-in in $StateDir"
            Remove-Item -LiteralPath $StateDir -Recurse -Force
        }
    }
    elseif (Test-Path -LiteralPath $StateDir) {
        Write-Host "Kept your identity and sign-in in $StateDir; installing again uses them."
        Write-Host "To delete them too, delete that folder."
    }

    Write-Host "URnetwork is uninstalled"
}

# Invoke-Installer downloads the installer from where this install came from
# and runs it in a child PowerShell, which keeps every setting.
function Invoke-Installer {
    param([String[]]$Arguments)
    $BaseUrl = Get-ScriptsBaseUrl
    $TempScriptPath = Join-Path ([IO.Path]::GetTempPath()) -ChildPath "urnetwork-installer-$PID.ps1"
    Write-Host "Downloading the installer from $BaseUrl"
    try {
        $ProgressPreference = "SilentlyContinue"
        Invoke-WebRequest -Uri "$BaseUrl/Provider_Install_Win32.ps1" -OutFile $TempScriptPath -UseBasicParsing
    }
    catch {
        throw "Couldn't download the installer ($($_.Exception.Message))"
    }

    # Run nothing that isn't a complete installer script.
    $Errors = $null
    $null = [System.Management.Automation.Language.Parser]::ParseFile($TempScriptPath, [ref]$null, [ref]$Errors)
    if ($Errors.Count -gt 0 -or (Get-Content -LiteralPath $TempScriptPath -Raw) -notmatch "Provider_Install_Win32") {
        Remove-Item -LiteralPath $TempScriptPath -Force -ErrorAction SilentlyContinue
        throw "The installer downloaded from $BaseUrl isn't a valid script; nothing was changed"
    }

    try {
        $AllArguments = @("-Destination", $InstalledPath, "-NonInteractive")
        if ($BaseUrl -ne $DefaultScriptsBaseUrl) {
            $AllArguments += @("-ScriptsBaseUrl", $BaseUrl)
        }
        & (Get-PowerShellPath) -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $TempScriptPath @AllArguments @Arguments | Out-Host
        return ($LASTEXITCODE -eq 0)
    }
    finally {
        Remove-Item -LiteralPath $TempScriptPath -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-Main {
    switch ($Command) {
        "uninstall" {
            Invoke-Uninstall
        }

        "update" {
            $InstalledTag = Get-InstalledTag
            $Tag = Get-LatestTag
            if ($InstalledTag -eq $Tag) {
                Write-Host "URnetwork is up to date ($InstalledTag)"
                return
            }
            Write-Host "Updating URnetwork from $InstalledTag to $Tag"
            if (-not (Invoke-Installer @())) {
                throw "The update didn't finish; $InstalledTag is still installed"
            }
            Write-Host "Update completed"
        }

        "version" {
            $InstalledTag = Get-InstalledTag
            if (-not $InstalledTag) {
                throw "Can't read the version file at $VersionPath. Reinstall URnetwork."
            }
            Write-Host "URnetwork version $InstalledTag"
            $Tag = Get-LatestTag
            if ($Tag -ne $InstalledTag) {
                Write-Host "A new release is out: $Tag. Run: urnet-tools update"
            }
            else {
                Write-Host "This is the newest release"
            }
        }

        "reinstall" {
            $Tag = $Version
            if (-not $Tag) {
                $Tag = Get-InstalledTag
            }
            if (-not $Tag) {
                $Tag = "latest"
            }
            Write-Host "Reinstalling URnetwork $Tag (settings and identity are kept)"
            if (-not (Invoke-Installer @("-Version", $Tag))) {
                throw "Reinstallation failed"
            }
            Write-Host "Reinstallation completed"
        }

        "auto-update" {
            switch ($Value) {
                "on" { Enable-AutoUpdate -NewInterval $Interval }
                "off" { Disable-AutoUpdate }
                default {
                    if (-not $Interval) {
                        Show-AutoUpdate
                    }
                    elseif ($OnWindows -and (Test-Path -LiteralPath $AutoUpdateShortcut)) {
                        Enable-AutoUpdate -NewInterval $Interval
                    }
                    else {
                        $NewInterval = ConvertTo-Interval $Interval
                        if (-not $NewInterval) {
                            throw "Unknown interval '$Interval'. Use 6h, day, week or month."
                        }
                        Set-Content -LiteralPath $IntervalPath -Value $NewInterval
                        Write-Host "Automatic updates will check every $NewInterval once turned on (they are off)"
                    }
                }
            }
        }

        "auto-update-enable" {
            Enable-AutoUpdate -NewInterval $Interval
        }

        "auto-update-disable" {
            Disable-AutoUpdate
        }

        "auto-update-freq" {
            $NewInterval = $Interval
            if (-not $NewInterval) {
                $NewInterval = Read-Host "How often should automatic updates check for a release? [6h/day/week/month]"
            }
            Enable-AutoUpdate -NewInterval $NewInterval
        }

        "auto-start" {
            switch ($Value) {
                "on" { Enable-AutoStart }
                "off" { Disable-AutoStart }
                default { Show-AutoStart }
            }
        }

        "auto-start-enable" {
            Enable-AutoStart
        }

        "auto-start-disable" {
            Disable-AutoStart
        }

        "start" {
            Start-Provider
        }

        "stop" {
            Stop-Provider
        }

        "restart" {
            Stop-Provider
            Start-Sleep -Seconds 1
            Start-Provider
        }

        "status" {
            $Running = Get-ProviderProcess
            if ($Running.Count -gt 0) {
                Write-Host "Status: running (process $($Running -join ', '))"
            }
            else {
                Write-Host "Status: stopped"
            }
            $InstalledTag = Get-InstalledTag
            if (-not $InstalledTag) {
                $InstalledTag = "unknown"
            }
            Write-Host "Version: $InstalledTag"
            Write-Host "Installed in: $InstalledPath"
            if (Test-SignedIn) {
                Write-Host "Signed in: yes ($StateDir)"
            }
            else {
                Write-Host "Signed in: no, run: urnetwork auth"
            }
            if (Test-Registered) {
                Write-Host "Device registered: yes"
            }
            elseif (Test-SignedIn) {
                Write-Host "Device registered: not yet, 'urnet-tools start' registers it"
            }
            Show-AutoStart
            Show-AutoUpdate
            if ($Running.Count -eq 0) {
                $script:ExitCode = 3
            }
        }

        "logs" {
            if (-not (Test-Path -LiteralPath $ProviderLogPath)) {
                throw "There's no log yet at $ProviderLogPath. It starts when 'urnet-tools start' runs the provider."
            }
            Write-Host "Following $ProviderLogPath (Ctrl-C to quit)"
            Get-Content -LiteralPath $ProviderLogPath -Tail 50 -Wait
        }
    }
}

$ExitCode = 0
try {
    Invoke-Main
}
catch {
    Write-Host "urnet-tools $($Command): $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
exit $ExitCode
