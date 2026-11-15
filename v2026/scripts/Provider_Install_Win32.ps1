#!/usr/bin/env pwsh
# Credits: Ar Rakin, Ryan Mello
# Provider_Install_Win32 -- installs the URnetwork provider for the current user
# GitHub: <https://github.com/urnetwork/connect>
<#
.SYNOPSIS
    Installs the URnetwork provider for the current Windows user.

.DESCRIPTION
    Downloads the provider from the urnetwork/build GitHub release, checks its
    SHA-256 digest, installs it as urnetwork.exe in
    %LOCALAPPDATA%\urnetwork\provider with urnet-tools, adds that folder to the
    user PATH, and turns on automatic updates. No administrator rights needed.

    Run it again at any time: it updates in place and keeps your settings and
    your identity (%USERPROFILE%\.urnetwork).

    Works in Windows PowerShell 5.1 and PowerShell 7, either saved and run with
        powershell -ExecutionPolicy Bypass -File .\Provider_Install_Win32.ps1
    or straight from the web with
        irm https://raw.githubusercontent.com/urnetwork/connect/main/scripts/Provider_Install_Win32.ps1 | iex

.PARAMETER Version
    Release tag to install (also -Tag or -t; 2026.10.20-1 means v2026.10.20-1).
    Defaults to the latest release. A version other than latest is installed
    without automatic updates.

.PARAMETER Destination
    Install folder. Defaults to %LOCALAPPDATA%\urnetwork\provider.

.PARAMETER NoAutoUpdate
    Don't turn on automatic updates (and turn them off if they were on).
    "urnet-tools auto-update on" turns them back on.

.PARAMETER AuthCode
    Sign in with this auth code (from https://ur.io) without a prompt. The
    URNETWORK_AUTH_CODE environment variable works too.

.PARAMETER NoStart
    Don't ask to sign in and don't start the provider.

.PARAMETER AddToStartup
    Start the provider in the background now and whenever you sign in to
    Windows, without asking.

.PARAMETER NoStartup
    Don't start the provider when you sign in (and remove that if it was set).

.PARAMETER NonInteractive
    Never ask a question. Sign-in is skipped unless -AuthCode is given, and the
    start-at-sign-in setting is left as it is unless -AddToStartup or
    -NoStartup says otherwise.

.PARAMETER NoRestartDownload
    Reuse a previous download of the same release if its digest still matches.

.PARAMETER NoCleanup
    Keep the downloaded archive and the extracted files.

.PARAMETER ToolsScriptPath
    Install urnet-tools.ps1 from this local file instead of downloading it.

.PARAMETER UpdaterScriptPath
    Install urnetwork-updater.ps1 from this local file instead of downloading it.

.PARAMETER ScriptsBaseUrl
    Where urnet-tools.ps1, urnetwork-updater.ps1 and this installer are
    downloaded from, now and by later updates. Defaults to urnetwork/connect
    main on GitHub. URNETWORK_TOOLS_URL (the URL of urnet-tools.ps1) works too.

.PARAMETER ReleaseApiUrl
    GitHub API URL of the repository whose releases hold the provider (for
    mirrors and testing). Defaults to https://api.github.com/repos/urnetwork/build,
    or $env:URNETWORK_RELEASE_API_URL when set.

.LINK
    https://ur.xyz/docs/miner
#>
param(
    [Alias("t", "Tag")]
    [String]$Version = "latest",
    [String]$Destination = "",
    [String]$ToolsScriptPath = "",
    [String]$UpdaterScriptPath = "",
    [String]$ScriptsBaseUrl = "",
    [String]$ReleaseApiUrl = "",
    [Switch]$NoRestartDownload = $false,
    [Switch]$NoCleanup = $false,
    [Switch]$NonInteractive = $false,
    [Switch]$AddToStartup = $false,
    [Switch]$NoStartup = $false,
    [Switch]$NoAutoUpdate = $false,
    [Switch]$NoStart = $false,
    [String]$AuthCode = "",
    [Switch]$Help = $false
);

$DefaultScriptsBaseUrl = "https://raw.githubusercontent.com/urnetwork/connect/refs/heads/main/scripts"
$DefaultReleaseApiUrl = "https://api.github.com/repos/urnetwork/build"

# Test-YesAnswer reads a [Y/n] prompt answer: yes is the default, so an empty
# answer (plain Enter), "y" and "yes" all mean yes.
function Test-YesAnswer {
    param([string]$Answer)
    $Normalized = "$Answer".Trim().ToLower()
    return ($Normalized -eq "" -or $Normalized -eq "y" -or $Normalized -eq "yes")
}

# Test-IsWindows is true on Windows PowerShell 5.1, where $IsWindows doesn't
# exist, and on PowerShell 7 for Windows.
function Test-IsWindows {
    return ($PSVersionTable.PSVersion.Major -lt 6 -or $IsWindows)
}

# Get-ProviderArch maps the machine to a release architecture. Win32_Processor
# reports the real CPU even when this PowerShell runs emulated (x64 PowerShell
# on an ARM64 PC); the environment variables are the fallback.
function Get-ProviderArch {
    if (Test-IsWindows) {
        $CpuArch = $null
        try {
            $CpuArch = @(Get-CimInstance -ClassName Win32_Processor -ErrorAction Stop)[0].Architecture
        }
        catch {}

        switch ($CpuArch) {
            9 { return "amd64" }
            12 { return "arm64" }
        }

        $EnvArch = $env:PROCESSOR_ARCHITEW6432
        if (-not $EnvArch) {
            $EnvArch = $env:PROCESSOR_ARCHITECTURE
        }

        switch ("$EnvArch".ToUpper()) {
            "AMD64" { return "amd64" }
            "ARM64" { return "arm64" }
        }

        return "unsupported ($EnvArch)"
    }

    $Machine = "$(uname -m)".Trim()
    switch ($Machine) {
        "x86_64" { return "amd64" }
        "aarch64" { return "arm64" }
        "arm64" { return "arm64" }
    }
    return "unsupported ($Machine)"
}

# Select-ProviderAsset picks the provider archive from a release. A file for
# just this platform (urnetwork-provider-<version>-<os>-<arch>.tar.gz) is
# preferred; otherwise the all-platform urnetwork-provider-<version>.tar.gz.
function Select-ProviderAsset {
    param(
        [Object[]]$Assets,
        [String]$OS,
        [String]$Arch
    )

    $PlatformPattern = "^urnetwork-provider-.+-$OS-$Arch\.tar\.gz$"
    $AnyPlatformPattern = "-(windows|linux|darwin)-[a-z0-9]+\.tar\.gz$"

    $Platform = @($Assets | Where-Object { $_.name -cmatch $PlatformPattern })
    if ($Platform.Count -gt 0) {
        return $Platform[0]
    }

    $All = @($Assets | Where-Object { $_.name -cmatch "^urnetwork-provider-.+\.tar\.gz$" -and $_.name -cnotmatch $AnyPlatformPattern })
    if ($All.Count -gt 0) {
        return $All[0]
    }

    return $null
}

# Get-AssetSha256 returns the hex SHA-256 GitHub publishes for a release file
# ("digest": "sha256:<hex>"), or "" when the release doesn't have one.
function Get-AssetSha256 {
    param([Object]$Asset)
    $Digest = "$($Asset.digest)"
    if ($Digest -match "^sha256:([0-9a-fA-F]{64})$") {
        return $Matches[1].ToLower()
    }
    return ""
}

function Test-FileSha256 {
    param(
        [String]$Path,
        [String]$Expected
    )
    if (-not (Test-Path -LiteralPath $Path)) {
        return $false
    }
    $Actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLower()
    return ($Actual -eq $Expected)
}

function Save-Download {
    param(
        [String]$URL,
        [String]$Destination
    );

    Write-Host "Downloading $URL"

    if ((Test-IsWindows) -and (Get-Command Start-BitsTransfer -ErrorAction SilentlyContinue)) {
        try {
            Start-BitsTransfer -Source $URL -Destination $Destination -ErrorAction Stop
            return
        }
        catch {
            Write-Host "Download with BITS didn't work ($($_.Exception.Message)), trying a normal web request"
        }
    }

    # The progress bar makes Windows PowerShell 5.1 downloads many times slower.
    $ProgressPreference = "SilentlyContinue"
    Invoke-WebRequest -Uri $URL -OutFile $Destination -UseBasicParsing
}

# Get-TarCommand prefers Windows' own bsdtar: a GNU tar earlier on PATH (Git
# for Windows ships one) reads "C:\..." as a remote host and fails.
function Get-TarCommand {
    if (Test-IsWindows) {
        $SystemTar = Join-Path $env:SystemRoot -ChildPath "System32\tar.exe"
        if (Test-Path -LiteralPath $SystemTar) {
            return $SystemTar
        }
    }

    $Tar = Get-Command tar -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($Tar) {
        return $Tar.Source
    }
    return ""
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

# Add-PathEntry returns PathValue with Entry appended, unchanged if it's there.
function Add-PathEntry {
    param(
        [String]$PathValue,
        [String]$Entry
    )
    $Entries = @("$PathValue".Split(";") | Where-Object { $_.Trim() -ne "" })
    foreach ($Existing in $Entries) {
        if (Test-SamePathEntry $Existing $Entry) {
            return "$PathValue"
        }
    }
    return (@($Entries) + @($Entry)) -join ";"
}

# The user PATH is read and written in the registry, not with
# [Environment]::SetEnvironmentVariable, which would save it as REG_SZ and
# expand entries such as %USERPROFILE%\bin into fixed paths for good.
function Get-UserPathRaw {
    $Key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment")
    if (-not $Key) {
        return ""
    }
    try {
        return [String]$Key.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    }
    finally {
        $Key.Close()
    }
}

function Set-UserPathRaw {
    param([String]$Value)
    $Key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey("Environment")
    try {
        $Key.SetValue("Path", $Value, [Microsoft.Win32.RegistryValueKind]::ExpandString)
    }
    finally {
        $Key.Close()
    }
}

# Get-ProviderProcess returns the ids of providers running "provide" from this
# install folder. Other runs of the same exe ("auth", or the miner's own
# "provider update" that may have started this install) are left alone.
function Get-ProviderProcess {
    param([String]$Folder)
    $Prefix = $Folder.TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    $Provide = '(^|\s)"?provide"?(\s|$)'
    if (Test-IsWindows) {
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

# Get-PowerShellPath is the PowerShell to run child scripts with: this one's
# edition, never the host (powershell_ise.exe under "irm | iex" in the ISE).
function Get-PowerShellPath {
    if ($PSVersionTable.PSEdition -eq "Core") {
        if (Test-IsWindows) {
            return Join-Path $PSHOME -ChildPath "pwsh.exe"
        }
        return Join-Path $PSHOME -ChildPath "pwsh"
    }
    return Join-Path $PSHOME -ChildPath "powershell.exe"
}

# Invoke-Tools runs the installed urnet-tools.ps1 in a child PowerShell, so the
# execution policy doesn't block it and its exit doesn't end this script.
function Invoke-Tools {
    param(
        [String]$ToolsPath,
        [String[]]$Arguments
    )
    & (Get-PowerShellPath) -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $ToolsPath @Arguments | Out-Host
    return ($LASTEXITCODE -eq 0)
}

function Install-ScriptFile {
    param(
        [String]$Name,
        [String]$LocalPath,
        [String]$BaseUrl,
        [String]$Target
    )
    $Temp = "$Target.download"
    if ($LocalPath) {
        Write-Host "Installing $Name from $LocalPath"
        Copy-Item -LiteralPath $LocalPath -Destination $Temp -Force
    }
    else {
        Write-Host "Installing $Name from $BaseUrl"
        $ProgressPreference = "SilentlyContinue"
        Invoke-WebRequest -Uri "$BaseUrl/$Name" -OutFile $Temp -UseBasicParsing
    }
    # A broken or wrong download (an HTML error page, a cut-off file) must not
    # replace a working script.
    $Errors = $null
    $null = [System.Management.Automation.Language.Parser]::ParseFile($Temp, [ref]$null, [ref]$Errors)
    $Text = Get-Content -LiteralPath $Temp -Raw
    if ($Errors.Count -gt 0 -or $Text -notmatch "github\.com/urnetwork/connect") {
        Remove-Item -LiteralPath $Temp -Force
        if (Test-Path -LiteralPath $Target) {
            Write-Host "Warning: the downloaded $Name isn't valid, keeping the installed one"
            return
        }
        throw "The downloaded $Name isn't a valid script"
    }
    Move-Item -LiteralPath $Temp -Destination $Target -Force
}

function Get-ReleaseInfo {
    param(
        [String]$ApiUrl,
        [String]$Tag
    )
    $Headers = @{ "Accept" = "application/vnd.github+json"; "User-Agent" = "urnetwork-installer" }
    if ($Tag -eq "latest") {
        $Urls = @("$ApiUrl/releases/latest")
    }
    elseif ($Tag.StartsWith("v")) {
        $Urls = @("$ApiUrl/releases/tags/$Tag")
    }
    else {
        # Release tags start with "v"; accept the version without it too.
        $Urls = @("$ApiUrl/releases/tags/$Tag", "$ApiUrl/releases/tags/v$Tag")
    }

    $LastError = ""
    foreach ($Url in $Urls) {
        try {
            return Invoke-RestMethod -Uri $Url -Headers $Headers -UseBasicParsing
        }
        catch {
            $LastError = $_.Exception.Message
        }
    }
    throw "Couldn't get release '$Tag' from $ApiUrl ($LastError). Check the version name and your internet connection. GitHub allows 60 of these checks an hour from one IP address, so if you ran this many times, wait an hour."
}

function Invoke-Install {
    $ErrorActionPreference = "Stop"

    if ($Help) {
        if ($PSCommandPath) {
            Get-Help $PSCommandPath -Detailed
        }
        else {
            Write-Host "Save this script and run: Get-Help .\Provider_Install_Win32.ps1 -Detailed"
        }
        return
    }

    if ($Version -match '[\\/]' -or $Version -match '\.\.') {
        throw "Version must not contain a slash, a backslash or '..'"
    }

    if ($AddToStartup -and $NoStartup) {
        throw "Use -AddToStartup or -NoStartup, not both"
    }

    $OnWindows = Test-IsWindows
    $OS = "windows"
    if (-not $OnWindows) {
        $OS = "linux"
        Write-Host "Note: this installer is for Windows. Other platforms only work for developing the script itself."
    }

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        # Windows PowerShell 5.1 may still default to TLS 1.0, which GitHub refuses.
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    }

    $Folder = $Destination
    if (-not $Folder) {
        if ($OnWindows) {
            $Folder = Join-Path -Path $env:LOCALAPPDATA -ChildPath "urnetwork\provider"
        }
        else {
            $Folder = Join-Path -Path $env:HOME -ChildPath ".local/share/urnetwork"
        }
    }
    # A trailing backslash would escape the closing quote in the Startup
    # shortcuts' arguments.
    $Folder = [IO.Path]::GetFullPath($Folder).TrimEnd('\', '/')

    $Arch = Get-ProviderArch
    if ($Arch -like "unsupported*") {
        throw "This PC's processor isn't supported: $Arch. URnetwork runs on x64 and ARM64 Windows."
    }

    $ApiUrl = $ReleaseApiUrl
    if (-not $ApiUrl) {
        $ApiUrl = $env:URNETWORK_RELEASE_API_URL
    }
    if (-not $ApiUrl) {
        $ApiUrl = $DefaultReleaseApiUrl
    }
    $ApiUrl = $ApiUrl.TrimEnd('/')

    $BaseUrl = $ScriptsBaseUrl
    $ScriptsUrlFile = Join-Path $Folder -ChildPath "scripts-url"
    if (-not $BaseUrl -and $env:URNETWORK_TOOLS_URL) {
        $BaseUrl = $env:URNETWORK_TOOLS_URL -replace "/urnet-tools\.ps1$", ""
    }
    if (-not $BaseUrl -and (Test-Path -LiteralPath $ScriptsUrlFile)) {
        $BaseUrl = "$(Get-Content -LiteralPath $ScriptsUrlFile -Raw)".Trim()
    }
    if (-not $BaseUrl) {
        $BaseUrl = $DefaultScriptsBaseUrl
    }
    $BaseUrl = $BaseUrl.TrimEnd('/')

    Write-Host "Installing URnetwork"
    Write-Host ""
    Write-Host "Version:      $Version"
    Write-Host "Destination:  $Folder"
    Write-Host "Architecture: $Arch"
    Write-Host ""

    $ReleaseInfo = Get-ReleaseInfo -ApiUrl $ApiUrl -Tag $Version
    $ReleaseVersion = "$($ReleaseInfo.tag_name)"
    if (-not $ReleaseVersion) {
        throw "The release information from $ApiUrl has no version tag"
    }

    $Asset = Select-ProviderAsset -Assets $ReleaseInfo.assets -OS $OS -Arch $Arch
    if (-not $Asset) {
        throw "Release $ReleaseVersion has no urnetwork-provider archive"
    }
    $ExpectedSha256 = Get-AssetSha256 $Asset

    $WorkDir = Join-Path ([IO.Path]::GetTempPath()) -ChildPath "urnetwork-install-$PID"
    $null = New-Item -Path $WorkDir -ItemType Directory -Force
    $ArchivePath = Join-Path $WorkDir -ChildPath $Asset.name
    $ExtractPath = Join-Path $WorkDir -ChildPath "extracted"

    try {
        # -NoRestartDownload reuses the archive an earlier run kept with -NoCleanup.
        $Earlier = Get-ChildItem -Path (Join-Path ([IO.Path]::GetTempPath()) -ChildPath "urnetwork-install-*\$($Asset.name)") -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($NoRestartDownload -and $Earlier -and -not (Test-Path -LiteralPath $ArchivePath)) {
            Copy-Item -LiteralPath $Earlier.FullName -Destination $ArchivePath
        }
        $Reuse = $NoRestartDownload -and (Test-Path -LiteralPath $ArchivePath) -and $ExpectedSha256 -and (Test-FileSha256 $ArchivePath $ExpectedSha256)
        if ($Reuse) {
            Write-Host "Reusing the earlier download $ArchivePath"
        }
        else {
            Remove-Item -LiteralPath $ArchivePath -Force -ErrorAction SilentlyContinue
            Save-Download -URL $Asset.browser_download_url -Destination $ArchivePath
        }

        if ($ExpectedSha256) {
            if (-not (Test-FileSha256 $ArchivePath $ExpectedSha256)) {
                Remove-Item -LiteralPath $ArchivePath -Force -ErrorAction SilentlyContinue
                throw "The download of $($Asset.name) is damaged: its SHA-256 doesn't match the release. Nothing was installed. Run the installer again."
            }
            Write-Host "Checked the SHA-256 of $($Asset.name)"
        }
        elseif ($env:URNETWORK_REQUIRE_CHECKSUM -eq "1") {
            # Set by the background updater: unattended, nothing is installed unchecked.
            throw "Release $ReleaseVersion doesn't publish a SHA-256 for $($Asset.name), so the automatic update skipped it. 'urnet-tools update' installs it by hand."
        }
        else {
            Write-Host "Warning: release $ReleaseVersion doesn't publish a SHA-256 for $($Asset.name), so the download can't be checked"
        }

        if (Test-Path -LiteralPath $ExtractPath) {
            Remove-Item -LiteralPath $ExtractPath -Recurse -Force
        }
        $null = New-Item -Path $ExtractPath -ItemType Directory

        $Tar = Get-TarCommand
        if (-not $Tar) {
            throw "tar.exe wasn't found. It comes with Windows 10 version 1803 and later; please update Windows."
        }

        Write-Host "Unpacking $($Asset.name)"
        & $Tar -xzf $ArchivePath -C $ExtractPath
        if ($LASTEXITCODE -ne 0) {
            throw "Couldn't unpack $ArchivePath (tar exit code $LASTEXITCODE). The disk may be full."
        }

        $BinarySuffix = ""
        if ($OnWindows) {
            $BinarySuffix = ".exe"
        }

        $Candidates = @(
            (Join-Path $ExtractPath -ChildPath "$OS/$Arch/provider$BinarySuffix"),
            (Join-Path $ExtractPath -ChildPath "provider$BinarySuffix")
        )
        if ($OnWindows -and $Arch -eq "arm64") {
            # Windows 11 on ARM runs x64 programs too.
            $Candidates += (Join-Path $ExtractPath -ChildPath "$OS/amd64/provider$BinarySuffix")
        }

        $BinaryPath = $Candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
        if (-not $BinaryPath) {
            throw "The release archive has no provider for $OS/$Arch"
        }
        if ($BinaryPath -like "*amd64*" -and $Arch -eq "arm64") {
            Write-Host "This release has no ARM64 build for Windows, installing the x64 build (Windows on ARM runs it emulated)"
        }

        if (-not $OnWindows) {
            & chmod +x $BinaryPath
        }
        # Make sure Windows can run it (right architecture, not blocked) before
        # it replaces anything.
        # (Windows PowerShell 5.1 would turn its stderr into an error under "Stop".)
        $ErrorActionPreference = "Continue"
        try {
            $null = & $BinaryPath --help 2>&1
        }
        catch {
            throw "The new provider doesn't run on this PC: $($_.Exception.Message)"
        }
        finally {
            $ErrorActionPreference = "Stop"
        }

        $null = New-Item -Path $Folder -ItemType Directory -Force

        $InstalledBinaryPath = Join-Path $Folder -ChildPath "urnetwork$BinarySuffix"
        # Only installs by the older scripts lack urnet-tools.cmd.
        $UpgradingOldInstall = (Test-Path -LiteralPath (Join-Path $Folder -ChildPath "version")) -and
            -not (Test-Path -LiteralPath (Join-Path $Folder -ChildPath "urnet-tools.cmd"))
        $InstalledToolsPath = Join-Path $Folder -ChildPath "urnet-tools.ps1"
        $InstalledUpdaterPath = Join-Path $Folder -ChildPath "urnetwork-updater.ps1"

        # The scripts first: if one can't be fetched, nothing else changed.
        Install-ScriptFile -Name "urnet-tools.ps1" -LocalPath $ToolsScriptPath -BaseUrl $BaseUrl -Target $InstalledToolsPath
        Install-ScriptFile -Name "urnetwork-updater.ps1" -LocalPath $UpdaterScriptPath -BaseUrl $BaseUrl -Target $InstalledUpdaterPath

        # Leftovers of earlier updates; one still in use stays until next time.
        Get-ChildItem -LiteralPath $Folder -Filter "urnetwork$BinarySuffix.old*" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue

        # Windows can't delete or overwrite a running program, but it can rename
        # it. So the running provider is moved aside, the new one put in place,
        # and only then is the running one restarted.
        $Running = Get-ProviderProcess $Folder
        $Aside = ""
        if (Test-Path -LiteralPath $InstalledBinaryPath) {
            $Aside = "$InstalledBinaryPath.old"
            if (Test-Path -LiteralPath $Aside) {
                $Aside = "$InstalledBinaryPath.old-$(Get-Random)"
            }
            Move-Item -LiteralPath $InstalledBinaryPath -Destination $Aside
        }

        Write-Host "Installing urnetwork$BinarySuffix ($ReleaseVersion)"
        try {
            Move-Item -LiteralPath $BinaryPath -Destination $InstalledBinaryPath
        }
        catch {
            if ($Aside) {
                Move-Item -LiteralPath $Aside -Destination $InstalledBinaryPath -ErrorAction SilentlyContinue
            }
            throw "Couldn't put the new urnetwork$BinarySuffix in place ($($_.Exception.Message)). The installed version is unchanged. If antivirus is scanning it, try again in a minute."
        }

        if ($OnWindows) {
            # Lets "urnet-tools" work from cmd.exe, and from PowerShell when its
            # execution policy blocks scripts.
            # One line, so cmd doesn't look for a next line after "uninstall"
            # deleted this file.
            $Shim = "@powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"%~dp0urnet-tools.ps1`" %* & exit /b`r`n"
            [IO.File]::WriteAllText((Join-Path $Folder -ChildPath "urnet-tools.cmd"), $Shim, (New-Object Text.UTF8Encoding($false)))
        }

        Set-Content -LiteralPath (Join-Path $Folder -ChildPath "version") -Value $ReleaseVersion
        $PublishedAt = ""
        if ($ReleaseInfo.published_at) {
            # PowerShell 7 already parses the date; 5.1 gives the string.
            $PublishedAt = ([DateTime]$ReleaseInfo.published_at).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ", [Globalization.CultureInfo]::InvariantCulture)
        }
        Set-Content -LiteralPath (Join-Path $Folder -ChildPath "date") -Value $PublishedAt
        if ($BaseUrl -ne $DefaultScriptsBaseUrl) {
            Set-Content -LiteralPath $ScriptsUrlFile -Value $BaseUrl
        }
        else {
            Remove-Item -LiteralPath $ScriptsUrlFile -Force -ErrorAction SilentlyContinue
        }

        if ($Running.Count -gt 0) {
            Write-Host "Restarting the running provider on the new version"
            $Running | ForEach-Object { Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue }
            Start-Sleep -Seconds 1
            $null = Invoke-Tools $InstalledToolsPath @("start")
        }
    }
    finally {
        if (-not $NoCleanup) {
            Remove-Item -LiteralPath $WorkDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    if ($OnWindows) {
        $PathValue = Get-UserPathRaw
        $NewPathValue = Add-PathEntry -PathValue $PathValue -Entry $Folder
        if ($NewPathValue -ne $PathValue) {
            Write-Host "Adding $Folder to your PATH"
            Set-UserPathRaw -Value $NewPathValue
        }
        # Setting a variable this way also tells Windows the environment
        # changed, so new terminals see the PATH without signing out.
        [Environment]::SetEnvironmentVariable("URNETWORK_INSTALL_DIR", $Folder, [EnvironmentVariableTarget]::User)
        $env:Path = Add-PathEntry -PathValue $env:Path -Entry $Folder
    }
    else {
        Write-Host "Not changing `$PATH here; add $Folder to it yourself"
    }

    # The state folder the background miner will use: the saved environment,
    # not a value set only in this window (see urnet-tools).
    if ($OnWindows) {
        $SavedStateDir = [Environment]::GetEnvironmentVariable("URNETWORK_STATE_DIR", [EnvironmentVariableTarget]::User)
        if (-not $SavedStateDir) {
            $SavedStateDir = [Environment]::GetEnvironmentVariable("URNETWORK_STATE_DIR", [EnvironmentVariableTarget]::Machine)
        }
        $env:URNETWORK_STATE_DIR = $SavedStateDir
    }
    $StateDir = $env:URNETWORK_STATE_DIR
    if (-not $StateDir) {
        $StateDir = Join-Path $HOME -ChildPath ".urnetwork"
    }
    $SignedIn = Test-Path -LiteralPath (Join-Path $StateDir -ChildPath "jwt")
    $Registered = @(Get-ChildItem -LiteralPath $StateDir -Force -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ".provider*.jwt" }).Count -gt 0

    $Code = $AuthCode
    if (-not $Code) {
        $Code = $env:URNETWORK_AUTH_CODE
    }

    if ($SignedIn) {
        Write-Host "Already signed in ($StateDir is kept)"
    }
    elseif ($Code -or (-not $NonInteractive -and -not $NoStart)) {
        for ($Attempt = 1; $Attempt -le 3; $Attempt++) {
            if (-not $Code) {
                Write-Host ""
                Write-Host "Sign in to start mining. Get an auth code at https://ur.io"
                $Code = "$(Read-Host "Auth code (or press Enter to do this later)")".Trim()
                if (-not $Code) {
                    break
                }
            }
            & $InstalledBinaryPath auth $Code
            $Code = ""
            if ($LASTEXITCODE -eq 0) {
                $SignedIn = $true
                break
            }
            if ($NonInteractive) {
                break
            }
            Write-Host "That didn't sign you in. Check the code and try again."
        }
        if (-not $SignedIn) {
            Write-Host "Not signed in yet. Sign in later with: urnetwork auth"
        }
    }

    $Settings = @()

    if (-not $OnWindows) {
        Write-Host "Automatic updates and start at sign-in are Windows only; skipping them here"
    }
    else {
        $UpdateShortcut = Join-Path ([Environment]::GetFolderPath("Startup")) -ChildPath "urnetwork-update.lnk"
        if ($UpgradingOldInstall) {
            # An older urnet-tools kept these settings only in the shortcut:
            # no shortcut means updates were turned off, and the shortcut's
            # -Frequency is the chosen interval.
            if (-not (Test-Path -LiteralPath $UpdateShortcut)) {
                Set-Content -LiteralPath (Join-Path $Folder -ChildPath "auto-update-off") -Value "Off in an earlier install"
            }
            else {
                try {
                    $OldArguments = (New-Object -ComObject WScript.Shell).CreateShortcut($UpdateShortcut).Arguments
                    if ($OldArguments -match "-Frequency every-(week|month)") {
                        Set-Content -LiteralPath (Join-Path $Folder -ChildPath "auto-update-interval") -Value $Matches[1]
                    }
                }
                catch {}
            }
        }
        $AutoUpdateOff = Test-Path -LiteralPath (Join-Path $Folder -ChildPath "auto-update-off")
        if ($NoAutoUpdate) {
            $null = Invoke-Tools $InstalledToolsPath @("auto-update", "off")
        }
        elseif ($AutoUpdateOff) {
            $Settings += "Automatic updates: off (you turned them off; 'urnet-tools auto-update on' turns them on)"
        }
        elseif ($Version -ne "latest") {
            if (Test-Path -LiteralPath $UpdateShortcut) {
                $Settings += "Automatic updates are on, so this moves to the newest release at the next check. To stay on ${ReleaseVersion}, run: urnet-tools auto-update off"
            }
            else {
                $Settings += "Automatic updates: not turned on, since you chose version $Version"
            }
        }
        elseif (-not (Invoke-Tools $InstalledToolsPath @("auto-update", "on"))) {
            $Settings += "Automatic updates: couldn't be turned on, see the message above"
        }

        $StartupShortcut = Join-Path ([Environment]::GetFolderPath("Startup")) -ChildPath "urnetwork.lnk"
        $AutoStart = Test-Path -LiteralPath $StartupShortcut

        if ($NoStartup) {
            if ($AutoStart) {
                $null = Invoke-Tools $InstalledToolsPath @("auto-start", "off")
            }
            $AutoStart = $false
        }
        elseif ($AddToStartup) {
            $AutoStart = $true
        }
        elseif (-not $AutoStart -and -not $NonInteractive -and -not $NoStart) {
            if ($SignedIn) {
                $AutoStart = Test-YesAnswer (Read-Host "Run URnetwork in the background, now and whenever you sign in to Windows? [Y/n]")
            }
            else {
                $Settings += "Once you've signed in, 'urnet-tools auto-start on' runs URnetwork whenever you sign in to Windows"
            }
        }

        if ($AutoStart) {
            # Also rewrites a shortcut an older installer made.
            if (Invoke-Tools $InstalledToolsPath @("auto-start", "on")) {
                # Updates (non-interactive) don't start a provider the person
                # stopped, but they do finish a device that never registered,
                # so an install stuck before registering heals on its own.
                $StartNow = $AddToStartup -or -not $NonInteractive -or -not $Registered
                if ($SignedIn -and -not $NoStart -and $StartNow -and (Get-ProviderProcess $Folder).Count -eq 0) {
                    $null = Invoke-Tools $InstalledToolsPath @("start")
                }
            }
        }
    }

    # In PowerShell, "urnet-tools" runs the .ps1, which a Restricted execution
    # policy blocks; the .cmd next to it always works.
    $Tools = "urnet-tools"
    if ($OnWindows) {
        # The policy a new window gets, so not this process's own (often
        # Bypass from the command line). Unset everywhere means Restricted.
        $Policy = "Restricted"
        foreach ($Scope in @("MachinePolicy", "UserPolicy", "CurrentUser", "LocalMachine")) {
            $ScopePolicy = "$(Get-ExecutionPolicy -Scope $Scope)"
            if ($ScopePolicy -ne "Undefined") {
                $Policy = $ScopePolicy
                break
            }
        }
        if ($Policy -eq "Restricted" -or $Policy -eq "AllSigned") {
            $Tools = "urnet-tools.cmd"
        }
    }

    $Bold = ""
    $Reset = ""
    if ($PSStyle) {
        $Bold = $PSStyle.Bold
        $Reset = $PSStyle.Reset
    }

    Write-Host ""
    Write-Host "URnetwork $ReleaseVersion is installed in $Folder"
    foreach ($Line in $Settings) {
        Write-Host $Line
    }
    Write-Host ""
    Write-Host "Open a new terminal window so it finds these commands:"
    Write-Host "$($Bold)Sign in:$($Reset)              urnetwork auth"
    Write-Host "$($Bold)Run in this window:$($Reset)   urnetwork provide"
    Write-Host "$($Bold)Run in background:$($Reset)    $Tools start   (also: stop, status)"
    Write-Host "$($Bold)Updates:$($Reset)              $Tools update   (auto-update on/off)"
    Write-Host "$($Bold)Uninstall:$($Reset)            $Tools uninstall   (keeps your identity)"
    Write-Host "$($Bold)More help:$($Reset)            urnetwork --help, $Tools help"
}

$WindowStateDir = $env:URNETWORK_STATE_DIR
try {
    Invoke-Install
    $ExitCode = 0
}
catch {
    Write-Host ""
    Write-Host "URnetwork wasn't installed: $($_.Exception.Message)" -ForegroundColor Red
    $ExitCode = 1
}
finally {
    # Under "irm | iex" this is the person's own session; leave it as it was.
    $env:URNETWORK_STATE_DIR = $WindowStateDir
}

# Run as a file, report the result as the exit code. Run with "irm | iex",
# don't: exit would close the person's PowerShell window.
if ($PSCommandPath) {
    exit $ExitCode
}
