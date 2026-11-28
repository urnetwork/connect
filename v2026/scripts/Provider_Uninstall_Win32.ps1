#!/usr/bin/env pwsh
# Provider_Uninstall_Win32 -- removes the URnetwork provider for the current user
# GitHub: <https://github.com/urnetwork/connect>
<#
.SYNOPSIS
    Uninstalls the URnetwork provider.

.DESCRIPTION
    Stops the provider and its updater, removes the Startup shortcuts, the
    PATH entry and the installed files. Your identity and sign-in
    (%USERPROFILE%\.urnetwork) are kept unless you add -RemoveIdentity, so
    installing again later is the same miner.

    It also cleans up the older layouts (urnetwork.exe under windows\<arch>,
    or C:\Program Files\urnetwork\provider with -ForAllUsers).

    "urnet-tools uninstall" does the same for the current layout.

.PARAMETER RemoveIdentity
    Also delete your identity and sign-in (%USERPROFILE%\.urnetwork). It asks
    you to type "delete" first. -RemoveData works too.

.PARAMETER Yes
    With -RemoveIdentity: don't ask. Without a person to ask and without -Yes,
    nothing is removed.

.PARAMETER ForAllUsers
    Remove an all-users install from C:\Program Files (needs an administrator PowerShell).

.PARAMETER InstalledPath
    Install folder. Defaults to %LOCALAPPDATA%\urnetwork\provider.
#>
param(
    [Alias("RemoveData")]
    [Switch]$RemoveIdentity = $false,
    [Switch]$Yes = $false,
    [Switch]$ForAllUsers = $false,
    [String]$InstalledPath = ""
);

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
        [String[]]$Entries
    )
    $Kept = @("$PathValue".Split(";") | Where-Object {
        $Current = $_
        $Current.Trim() -ne "" -and -not ($Entries | Where-Object { Test-SamePathEntry $Current $_ })
    })
    return $Kept -join ";"
}

function Invoke-Uninstall {
    $ErrorActionPreference = "Stop"

    if ($ForAllUsers) {
        $CurrentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
        if (-not $CurrentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
            throw "-ForAllUsers needs an administrator PowerShell (right-click, Run as administrator)"
        }
    }

    $Folder = $InstalledPath
    if (-not $Folder) {
        if ($ForAllUsers) {
            $Folder = Join-Path $env:ProgramFiles -ChildPath "urnetwork\provider"
        }
        else {
            $Folder = Join-Path $env:LOCALAPPDATA -ChildPath "urnetwork\provider"
        }
    }
    $Folder = [IO.Path]::GetFullPath($Folder).TrimEnd('\', '/')

    $StateDir = $env:URNETWORK_STATE_DIR
    if (-not $StateDir) {
        $StateDir = Join-Path $HOME -ChildPath ".urnetwork"
    }

    if ($RemoveIdentity -and -not $Yes -and (Test-Path -LiteralPath $StateDir)) {
        if ([Console]::IsInputRedirected -or -not [Environment]::UserInteractive) {
            throw "-RemoveIdentity deletes $StateDir for good. Add -Yes to do that without a question. Nothing was removed."
        }
        Write-Host "This deletes your URnetwork identity and sign-in in $StateDir. It can't be undone."
        if ("$(Read-Host "Type delete to confirm")".Trim() -ne "delete") {
            throw "Not confirmed. Nothing was removed."
        }
    }

    $StartupFolders = @([Environment]::GetFolderPath("Startup"))
    if ($ForAllUsers) {
        $StartupFolders += [Environment]::GetFolderPath("CommonStartup")
    }

    # Only ever delete a folder that holds an URnetwork install, and never one
    # that holds the identity.
    $Markers = @("urnetwork.exe", "urnet-tools.ps1", "windows\amd64\urnetwork.exe", "windows\arm64\urnetwork.exe")
    $Found = $false
    if (Test-Path -LiteralPath $Folder) {
        $Found = @($Markers | Where-Object { Test-Path -LiteralPath (Join-Path $Folder -ChildPath $_) }).Count -gt 0
        if (-not $Found) {
            throw "$Folder doesn't look like an URnetwork install (no urnetwork.exe or urnet-tools.ps1), so nothing was removed"
        }
        $FullState = [IO.Path]::GetFullPath($StateDir).TrimEnd('\') + "\"
        if ($FullState.StartsWith($Folder + "\", [StringComparison]::OrdinalIgnoreCase)) {
            throw "$Folder contains your identity folder $StateDir, so nothing was removed"
        }
    }
    foreach ($StartupFolder in $StartupFolders) {
        foreach ($Name in @("urnetwork.lnk", "urnetwork-update.lnk")) {
            $Shortcut = Join-Path $StartupFolder -ChildPath $Name
            if (Test-Path -LiteralPath $Shortcut) {
                Write-Host "Removing startup entry: $Shortcut"
                Remove-Item -LiteralPath $Shortcut -Force
                $Found = $true
            }
        }
    }

    if (-not $Found) {
        throw "No URnetwork installation was found in $Folder"
    }

    # The background updater, then every provider started from the folder,
    # including the older windows\<arch> layout.
    $Processes = @()
    try {
        $Processes = @(Get-CimInstance -ClassName Win32_Process -ErrorAction Stop)
    }
    catch {}

    foreach ($Process in $Processes) {
        $IsUpdater = $Process.CommandLine -and $Process.CommandLine -like "*urnetwork-updater.ps1*"
        $IsProvider = $Process.ExecutablePath -and $Process.ExecutablePath.StartsWith($Folder + "\", [StringComparison]::OrdinalIgnoreCase)
        if (($IsUpdater -or $IsProvider) -and $Process.ProcessId -ne $PID) {
            Write-Host "Stopping $($Process.Name) (process $($Process.ProcessId))"
            Stop-Process -Id $Process.ProcessId -Force -ErrorAction SilentlyContinue
        }
    }
    Start-Sleep -Seconds 1

    # Only the installers' own files are deleted; the folder goes when it's empty.
    if ($Found -and (Test-Path -LiteralPath $Folder)) {
        Write-Host "Removing URnetwork from $Folder"
        $Files = @("urnetwork.exe", "urnet-tools.ps1", "urnet-tools.cmd", "urnetwork-updater.ps1", "version", "date",
            "scripts-url", "auto-update-off", "auto-update-interval", "last-update-check", "updater.log",
            "urnetwork-updater.pid", "provider.log", "provider.log.1", "provider.log.err")
        foreach ($File in $Files) {
            Remove-Item -LiteralPath (Join-Path $Folder -ChildPath $File) -Force -ErrorAction SilentlyContinue
        }
        Get-ChildItem -LiteralPath $Folder -Filter "urnetwork.exe.old*" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
        foreach ($OldArch in @("amd64", "arm64")) {
            Remove-Item -LiteralPath (Join-Path $Folder -ChildPath "windows\$OldArch\urnetwork.exe") -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath (Join-Path $Folder -ChildPath "windows\$OldArch") -Force -ErrorAction SilentlyContinue
        }
        Remove-Item -LiteralPath (Join-Path $Folder -ChildPath "windows") -Force -ErrorAction SilentlyContinue
        if (Test-Path -LiteralPath (Join-Path $Folder -ChildPath "urnetwork.exe")) {
            throw "Couldn't remove $Folder\urnetwork.exe. Close any window running URnetwork and try again."
        }
        if (-not (Get-ChildItem -LiteralPath $Folder -Force -ErrorAction SilentlyContinue)) {
            Remove-Item -LiteralPath $Folder -Force -ErrorAction SilentlyContinue
            $Parent = Split-Path -Parent $Folder
            if ((Split-Path -Leaf $Parent) -eq "urnetwork" -and -not (Get-ChildItem -LiteralPath $Parent -Force -ErrorAction SilentlyContinue)) {
                Remove-Item -LiteralPath $Parent -Force -ErrorAction SilentlyContinue
            }
        }
        else {
            Write-Host "Left $Folder in place: it has other files in it"
        }
    }

    # Current layout, and the older windows\<arch> one.
    $Entries = @($Folder, "$Folder\windows\amd64", "$Folder\windows\arm64")
    $Target = [EnvironmentVariableTarget]::User
    $Hive = [Microsoft.Win32.Registry]::CurrentUser
    $KeyName = "Environment"
    if ($ForAllUsers) {
        $Target = [EnvironmentVariableTarget]::Machine
        $Hive = [Microsoft.Win32.Registry]::LocalMachine
        $KeyName = "SYSTEM\CurrentControlSet\Control\Session Manager\Environment"
    }

    # Changed in the registry so entries like %USERPROFILE%\bin stay as they are.
    $Key = $Hive.OpenSubKey($KeyName, $true)
    if ($Key) {
        try {
            $PathValue = [String]$Key.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            $NewPathValue = Remove-PathEntry -PathValue $PathValue -Entries $Entries
            if ($NewPathValue -ne $PathValue) {
                Write-Host "Removing URnetwork from PATH"
                $Key.SetValue("Path", $NewPathValue, [Microsoft.Win32.RegistryValueKind]::ExpandString)
            }
        }
        finally {
            $Key.Close()
        }
    }
    # Removing a variable this way also tells Windows the environment changed.
    [Environment]::SetEnvironmentVariable("URNETWORK_INSTALL_DIR", $null, $Target)

    if ($RemoveIdentity) {
        if (Test-Path -LiteralPath $StateDir) {
            Write-Host "Deleting your identity and sign-in: $StateDir"
            Remove-Item -LiteralPath $StateDir -Recurse -Force
        }
    }
    elseif (Test-Path -LiteralPath $StateDir) {
        Write-Host "Kept your identity and sign-in in $StateDir; installing again uses them."
        Write-Host "To delete them too, delete that folder."
    }

    Write-Host "URnetwork is uninstalled."
}

try {
    Invoke-Uninstall
    $ExitCode = 0
}
catch {
    Write-Host "URnetwork wasn't uninstalled: $($_.Exception.Message)" -ForegroundColor Red
    $ExitCode = 1
}

# Run with "irm | iex", exit would close the person's PowerShell window.
if ($PSCommandPath) {
    exit $ExitCode
}
