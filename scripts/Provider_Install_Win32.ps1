#!/usr/bin/env pwsh

param(
    [String]$Version = "latest",
    [Switch]$ForAllUsers = $false
);

if ($Version -contains "/") {
    Write-Error "Version should not contain a slash. Use the tag name instead."
    exit 1
}

$CurrentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())

if ($ForAllUsers) {
    if (-not $CurrentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Write-Host "Not running as admin. Please run with elevated admin permissions."
        exit 1
    }
    else {
        Write-Host "Running as admin"
    }
}

$Arch = (Get-CimInstance Win32_Processor).Architecture
$Arch = switch ($Arch) {
    9 { "amd64" }
    12 { "arm64" }
    default { "unsupported" }
}

Write-Debug "Detected architecture: $Arch"

if ($Arch -eq "unsupported") {
    Write-Error "Unsupported architecture. Urnetwork only supports Windows on x64 and ARM64."
    exit 1
}

$ApiUrl = "https://api.github.com/repos/urnetwork/build/releases/"

if ($Version -eq "latest") {
    $ApiUrl += "latest"
}
else {
    $ApiUrl += "tags/$Version"
}

$ReleaseInfo = Invoke-RestMethod -Uri "$ApiUrl"

if (!$ReleaseInfo) {
  Write-Error "Failed to fetch release information from GitHub API. Are you sure the version exists and your internet connection is working?"
  exit 1
}

$ReleaseAsset = $ReleaseInfo.assets | Where-Object { $_.name -cmatch "^urnetwork-provider-" }

if (!$ReleaseAsset) {
  Write-Error "Failed to find the release asset. Are you sure the version exists?"
  exit 1
}

$DownloadUrl = $ReleaseAsset.browser_download_url
$FileName = $ReleaseAsset.name
$FilePath = Join-Path -Path $env:TEMP -ChildPath $FileName

Write-Host "Downloading $FileName from $DownloadUrl"
try {
    Start-BitsTransfer -Source $DownloadUrl -Destination $FilePath -ErrorAction Stop
}
catch {
    Write-Warning "Something went wrong during the download process. Reattempting using different technique."
    try {
        Invoke-WebRequest -Uri $DownloadUrl -OutFile $FilePath -UseBasicParsing -ErrorAction Stop
    }
    catch {
        Write-Error "Failed to download the release asset. Are you sure the version exists and your internet connection is working?"
        exit 1
    }
}

$InstallDir = Join-Path -Path $env:LOCALAPPDATA -ChildPath "urnetwork\provider"

if ($ForAllUsers) {
    $InstallDir = "C:\Program Files\urnetwork\provider"
}

Write-Host "Installation directory: $InstallDir"

$ProviderExe = "$InstallDir\windows\$Arch\urnetwork.exe"

if (Test-Path $InstallDir) {
    Write-Host "Removing old version from $InstallDir"
    Get-WmiObject Win32_Process | Where-Object { $_.ExecutablePath -eq $ProviderExe } | ForEach-Object { $_.Terminate() }
    Remove-Item -Path $InstallDir -Recurse -Force
}

Write-Host "Creating directory $InstallDir"
New-Item -Path $InstallDir -ItemType Directory

if (!$?) {
    Write-Error "Failed to create directory $InstallDir. Please check your permissions."
    exit 1
}

Write-Host "Extracting $FileName to $InstallDir"
tar -xzf "$FilePath" -C "$InstallDir"

if (!$?) {
    Write-Error "Failed to extract the release asset. Are you sure the version exists?"
    exit 1
}

Remove-Item -Path "$InstallDir\darwin" -Recurse -Force
Remove-Item -Path "$InstallDir\linux" -Recurse -Force

if ($Arch -eq "amd64") {
    Remove-Item -Path "$InstallDir\windows\arm64" -Recurse -Force
}

if ($Arch -eq "arm64") {
    Remove-Item -Path "$InstallDir\windows\amd64" -Recurse -Force
}

Remove-Item -Path "$InstallDir\windows\$Arch\._provider.exe" -Force
Rename-Item -Path "$InstallDir\windows\$Arch\provider.exe" "urnetwork.exe"

Write-Host "Cleaning up temporary files"
Remove-Item -Path $FilePath -Force

if (!$?) {
    Write-Error "Failed to remove temporary files. Please check your permissions."
    exit 1
}

function Get-Path {
    if ($ForAllUsers) {
        return [System.Environment]::GetEnvironmentVariable("PATH", [System.EnvironmentVariableTarget]::Machine)
    }

    return [Environment]::GetEnvironmentVariable("PATH", [System.EnvironmentVariableTarget]::User)
}

function Set-Path {
    param (
        [Parameter(Mandatory = $true)]
        [String]$Value
    )

    if ($ForAllUsers) {
        [System.Environment]::SetEnvironmentVariable("PATH", $Value, [System.EnvironmentVariableTarget]::Machine)
    }
    else {
        [Environment]::SetEnvironmentVariable("PATH", $Value, [System.EnvironmentVariableTarget]::User)
    }
}

$EnvPath = Get-Path
$EnvPathSplitted = $EnvPath.Split(";")

if (-not ($EnvPathSplitted -contains "$InstallDir\windows\$Arch")) {
    Write-Host "Updating PATH variable"

    $Colon = ";";

    if ($EnvPath -match ";$") {
        $Colon = "";
    }

    $NewPath = "$EnvPath$Colon$InstallDir\windows\$Arch;"
    Set-Path -Value $NewPath

    if (!$?) {
        Write-Error "Failed to update PATH variable. Please check your permissions."
        exit 1
    }
}

Write-Host "Installation complete."

$DataDir = "$env:HOMEDRIVE$env:HOMEPATH\.urnetwork"

if (Test-Path $DataDir) {
    Write-Host "Found data directory at $DataDir"
    Write-Host "Skipping authentication"
}
else {
    $AuthNow = Read-Host "Do you want to authenticate now? (Y/n)"

    if ($AuthNow.ToLower() -ne "n") {
        while ($true) {
            & $ProviderExe auth

            if ($?) {
                break
            }
        }
    }
    else {
        Write-Host "Skipping authentication. Please run 'urnetwork auth' to authenticate later using an auth code."
    }
}

$AddToStartup = Read-Host "Do you want to add this service to startup? (Y/n)"

if ($AddToStartup.ToLower() -ne "n") {
    $StartupPath = Join-Path -Path $env:APPDATA -ChildPath "Microsoft\Windows\Start Menu\Programs\Startup"

    if ($ForAllUsers) {
        $StartupPath = Join-Path -Path "C:\ProgramData" -ChildPath "Microsoft\Windows\Start Menu\Programs\Startup"
    }

    $ShortcutPath = Join-Path -Path $StartupPath -ChildPath "urnetwork.lnk"

    if (Test-Path $ShortcutPath) {
        Remove-Item -Path $ShortcutPath -Force
    }

    $StartCommand = "Start-Process -FilePath '$ProviderExe' -ArgumentList 'provide' -WindowStyle Hidden"
    $Arguments = '-NoProfile -WindowStyle Hidden -Command "' + $StartCommand + '"'

    Write-Host "Startup command: powershell.exe $Arguments"

    $WshShell = New-Object -ComObject WScript.Shell
    $Shortcut = $WshShell.CreateShortcut($ShortcutPath)
    $Shortcut.TargetPath = "powershell.exe"
    $Shortcut.Arguments = $Arguments
    $Shortcut.WorkingDirectory = "$InstallDir\windows\$Arch"
    $Shortcut.WindowStyle = 7
    $Shortcut.IconLocation = $ProviderExe
    $Shortcut.Save()
    
    Write-Host "Added urnetwork provider to startup."
}

Write-Host "Please restart your command line for the changes to take effect."
Write-Host "You can now use the urnetwork provider by running 'urnetwork' in your command line."
