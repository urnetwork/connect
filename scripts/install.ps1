<#
.SYNOPSIS
    Unified Installer, Uninstaller, and Manager for the Urnetwork Provider on Windows.
.DESCRIPTION
    This script handles the complete lifecycle of the urnetwork provider binary. It supports
    initial installation, seamless upgrades from legacy versions, manual/automatic updates,
    and complete uninstallation.
.LINK
    https://ur.io/
#>
[CmdletBinding(DefaultParameterSetName='Install', SupportsShouldProcess=$true)]
param(
    [Parameter(Mandatory=$false, ParameterSetName='Install')]
    [string]$Version = 'latest',

    [Parameter(Mandatory=$false, ParameterSetName='Install')]
    [Parameter(Mandatory=$false, ParameterSetName='Uninstall')]
    [switch]$ForAllUsers,

    [Parameter(Mandatory=$true, ParameterSetName='Update')]
    [switch]$Update,

    [Parameter(Mandatory=$true, ParameterSetName='Uninstall')]
    [switch]$Uninstall,

    [Parameter(Mandatory=$true, ParameterSetName='Version')]
    [switch]$ShowVersion
)

begin {
    # --- Script Configuration and Variables ---
    $ScriptVersion = "2.0.0"
    $GithubRepo = "urnetwork/build"
    $ProviderProcessName = "urnetwork"
    $ManagerScriptName = "urnetwork-manager.ps1"
    $ProviderExeName = "urnetwork.exe"

    # --- Determine Installation Scope and Paths ---
    if ($ForAllUsers) {
        $InstallScope = [System.EnvironmentVariableTarget]::Machine
        $UrnetworkHome = Join-Path -Path $env:ProgramData -ChildPath "urnetwork"
        $OldInstallDir = Join-Path -Path ${env:ProgramFiles} -ChildPath "urnetwork\provider"
    } else {
        $InstallScope = [System.EnvironmentVariableTarget]::User
        $UrnetworkHome = Join-Path -Path $env:LOCALAPPDATA -ChildPath "urnetwork"
        $OldInstallDir = Join-Path -Path $env:LOCALAPPDATA -ChildPath "urnetwork\provider"
    }

    $BinDir = Join-Path -Path $UrnetworkHome -ChildPath "bin"
    $LogsDir = Join-Path -Path $UrnetworkHome -ChildPath "logs"
    $ManagerScriptPath = Join-Path -Path $BinDir -ChildPath $ManagerScriptName
    $ProviderExePath = Join-Path -Path $BinDir -ChildPath $ProviderExeName
    $VersionFile = Join-Path -Path $UrnetworkHome -ChildPath "version.txt"
    $InstallMarkerFile = Join-Path -Path $UrnetworkHome -ChildPath "install.cfg"
    $UserDataDir = Join-Path -Path $env:USERPROFILE -ChildPath ".urnetwork"

    # Scheduled Task configuration
    $ProviderTaskName = "Urnetwork Provider"
    $UpdateTaskName = "Urnetwork Provider Update Check"

    # --- Helper Functions ---
    function Write-Log {
        param([string]$Message, [string]$Level = "INFO")
        $Color = switch ($Level) {
            "INFO"    { "Cyan" }
            "SUCCESS" { "Green" }
            "WARN"    { "Yellow" }
            "ERROR"   { "Red" }
        }
        Write-Host "[$Level] $Message" -ForegroundColor $Color
    }

    function Test-IsAdmin {
        $identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
        $principal = New-Object System.Security.Principal.WindowsPrincipal($identity)
        return $principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)
    }

    function Get-Architecture {
        $arch = (Get-CimInstance -ClassName Win32_Processor).Architecture
        return switch ($arch) {
            9 { "amd64" }
            12 { "arm64" }
            default { throw "Unsupported architecture detected. Only x64 and ARM64 are supported." }
        }
    }

    function Get-GitHubRelease {
        param([string]$Tag)
        $apiUrl = "https://api.github.com/repos/$GithubRepo/releases"
        if ($Tag -eq 'latest') { $apiUrl += "/latest" } 
        else { $apiUrl += "/tags/$Tag" }
        
        try {
            Write-Log "Fetching release info for tag '$Tag' from GitHub..."
            $release = Invoke-RestMethod -Uri $apiUrl -UseBasicParsing
            return $release
        } catch {
            Write-Log "Failed to fetch release info. Check your internet connection or the tag name." -Level ERROR
            throw $_
        }
    }
    
    function Manage-Path {
        param([switch]$Add, [switch]$Remove)
        
        $currentPath = [System.Environment]::GetEnvironmentVariable('PATH', $InstallScope)
        $pathItems = $currentPath -split ';' | Where-Object { $_ -ne '' }

        # Clean up legacy paths first
        $pathItems = $pathItems | Where-Object { $_ -notlike "*urnetwork\provider*" }

        # Remove our bin dir if it exists
        $pathItems = $pathItems | Where-Object { $_ -ne $BinDir }

        if ($Add) {
            $pathItems += $BinDir
        }
        
        $newPath = ($pathItems | Select-Object -Unique) -join ';'
        
        if ($newPath -ne $currentPath) {
            Write-Log "Updating PATH variable for scope: $InstallScope"
            [System.Environment]::SetEnvironmentVariable('PATH', $newPath, $InstallScope)
            # Broadcast the change to other processes
            $HWND_BROADCAST = [IntPtr]0xffff
            $WM_SETTINGCHANGE = 0x1a
            [void] [System.Runtime.InteropServices.Marshal]::GetDelegateForFunctionPointer(
                [System.Runtime.InteropServices.Marshal]::GetFunctionPointer(
                    [System.AppDomain]::CurrentDomain.GetAssemblies() | Where-Object { $_.Location -and (Split-Path $_.Location -Leaf) -eq 'System.dll' } | Get-Member -Name SendMessageTimeout -MemberType Method -Static | Select-Object -First 1
                ), [System.Action[IntPtr, int, string, int, int, int, IntPtr]]
            ).Invoke($HWND_BROADCAST, $WM_SETTINGCHANGE, "ENVIRONMENT", 0, 5000, 0, [IntPtr]::Zero)
        }
    }
    
    function Manage-ScheduledTask {
        param([switch]$Add, [switch]$Remove)
        
        if ($Remove) {
            Get-ScheduledTask -TaskName $ProviderTaskName -ErrorAction SilentlyContinue | Unregister-ScheduledTask -Confirm:$false
            Get-ScheduledTask -TaskName $UpdateTaskName -ErrorAction SilentlyContinue | Unregister-ScheduledTask -Confirm:$false
            return
        }

        if ($Add) {
            # Task to run the provider
            $actionProvider = New-ScheduledTaskAction -Execute $ProviderExePath -Argument "provide"
            $triggerProvider = New-ScheduledTaskTrigger -AtLogOn
            $principalProvider = New-ScheduledTaskPrincipal -UserId (Get-CimInstance Win32_ComputerSystem).UserName -LogonType Interactive
            $settingsProvider = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Days 9999)
            Register-ScheduledTask -TaskName $ProviderTaskName -Action $actionProvider -Trigger $triggerProvider -Principal $principalProvider -Settings $settingsProvider -Force -Description "Runs the Urnetwork Provider at user logon."
            
            # Task to run weekly updates
            $actionUpdate = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NonInteractive -WindowStyle Hidden -File `"$ManagerScriptPath`" -Update"
            $triggerUpdate = New-ScheduledTaskTrigger -Weekly -DaysOfWeek Sunday -At '3AM'
            $principalUpdate = New-ScheduledTaskPrincipal -UserId (Get-CimInstance Win32_ComputerSystem).UserName -LogonType Interactive
            Register-ScheduledTask -TaskName $UpdateTaskName -Action $actionUpdate -Trigger $triggerUpdate -Principal $principalUpdate -Force -Description "Checks for Urnetwork Provider updates weekly."
        }
    }
}

process {
    # --- Bootstrapping: If called from web, delegate to local manager if it exists ---
    if ((Test-Path $ManagerScriptPath) -and ($MyInvocation.MyCommand.Path -ne $ManagerScriptPath)) {
        Write-Log "Local manager found. Delegating command..."
        & $ManagerScriptPath @PSBoundParameters
        return
    }

    if ($PSCmdlet.ParameterSetName -eq "Install") {
        Invoke-Install -Version $Version
    } elseif ($PSCmdlet.ParameterSetName -eq "Uninstall") {
        Invoke-Uninstall
    } elseif ($PSCmdlet.ParameterSetName -eq "Update") {
        Invoke-Update
    } elseif ($PSCmdlet.ParameterSetName -eq "Version") {
        Get-VersionInfo
    }
}

end {
    function Invoke-Install {
        [CmdletBinding()]
        param([string]$Version)

        if ($ForAllUsers -and -not (Test-IsAdmin)) {
            throw "Installation for all users requires administrative privileges. Please re-run from an elevated PowerShell."
        }

        $isUpgrade = $false
        if (Test-Path $OldInstallDir) {
            $isUpgrade = $true
            Write-Log "Legacy installation found. Beginning upgrade process."
        } elseif (Test-Path $ProviderExePath) {
            Write-Log "Urnetwork Provider is already installed. Checking for updates." -Level INFO
            Invoke-Update
            return
        }
        
        Write-Log "Starting Urnetwork Provider installation..."
        $arch = Get-Architecture
        Write-Log "Detected Architecture: $arch"
        
        # --- Download and Extract ---
        $release = Get-GitHubRelease -Tag $Version
        $asset = $release.assets | Where-Object { $_.name -like 'urnetwork-provider-*.tar.gz' } | Select-Object -First 1
        if (-not $asset) { throw "Could not find a suitable release asset for tag '$Version'." }

        $downloadUrl = $asset.browser_download_url
        $releaseTag = $release.tag_name
        
        Write-Log "Downloading Urnetwork Provider ($releaseTag)..."
        $tempFile = Join-Path -Path $env:TEMP -ChildPath $asset.name
        Invoke-WebRequest -Uri $downloadUrl -OutFile $tempFile -UseBasicParsing
        
        Write-Log "Extracting archive..."
        $tempExtractDir = Join-Path -Path $env:TEMP -ChildPath "urnetwork-extract"
        if (Test-Path $tempExtractDir) { Remove-Item -Path $tempExtractDir -Recurse -Force }
        Expand-Archive -Path $tempFile -DestinationPath $tempExtractDir -Force
        
        # --- Stop existing process and place binaries ---
        Stop-Process -Name $ProviderProcessName -ErrorAction SilentlyContinue
        
        New-Item -Path $BinDir -ItemType Directory -Force | Out-Null
        New-Item -Path $LogsDir -ItemType Directory -Force | Out-Null
        
        $binarySourcePath = Join-Path -Path $tempExtractDir -ChildPath "windows\$arch\provider.exe"
        if (-not (Test-Path $binarySourcePath)) { throw "Provider binary not found in archive for architecture '$arch'." }
        
        Write-Log "Installing binary to $ProviderExePath"
        Move-Item -Path $binarySourcePath -Destination $ProviderExePath -Force
        Set-Content -Path $VersionFile -Value $releaseTag
        
        # --- Install Manager and Persistence ---
        Write-Log "Installing manager script to $ManagerScriptPath"
        if ($MyInvocation.MyCommand.Path -ne $ManagerScriptPath) {
            Copy-Item -Path $MyInvocation.MyCommand.Path -Destination $ManagerScriptPath -Force
        }
        
        # --- Clean up legacy startup items ---
        if ($isUpgrade) {
            $oldStartupShortcut = Join-Path -Path ([Environment]::GetFolderPath('Startup')) -ChildPath "urnetwork.lnk"
            $oldAllUsersStartup = Join-Path -Path ([Environment]::GetFolderPath('CommonStartup')) -ChildPath "urnetwork.lnk"
            Remove-Item -Path $oldStartupShortcut -ErrorAction SilentlyContinue
            Remove-Item -Path $oldAllUsersStartup -ErrorAction SilentlyContinue
        }
        
        Write-Log "Creating Scheduled Tasks for provider and auto-updates..."
        Manage-ScheduledTask -Add
        
        Write-Log "Updating PATH environment variable..."
        Manage-Path -Add
        
        # --- Cleanup ---
        Remove-Item -Path $tempFile, $tempExtractDir -Recurse -Force
        if ($isUpgrade) {
            Write-Log "Cleaning up legacy installation directory..."
            Remove-Item -Path $OldInstallDir -Recurse -Force -ErrorAction SilentlyContinue
        }

        # Create a marker file to remember the installation scope
        Set-Content -Path $InstallMarkerFile -Value "Scope=$($InstallScope.ToString())"

        # --- Final Instructions ---
        Write-Log "Installation Successful!" -Level SUCCESS
        Write-Log "Version $releaseTag has been installed."
        Write-Log "A new PowerShell session is required for PATH changes to apply."
        Write-Log "The provider will start automatically on next logon. You can start it now with:"
        Write-Host "  `$ urnetwork provide" -ForegroundColor White
        Write-Log "To authenticate, run:"
        Write-Host "  `$ urnetwork auth" -ForegroundColor White
        Write-Log "Manage your installation with 'urnetwork-manager.ps1 -Update' or '-Uninstall'."
    }

    function Invoke-Uninstall {
        Write-Log "Starting uninstallation of Urnetwork Provider..."
        if (-not (Test-Path $UrnetworkHome)) {
            Write-Log "Urnetwork installation not found. Attempting legacy cleanup." -Level WARN
            Manage-Path -Remove
            Write-Log "Uninstallation finished." -Level SUCCESS
            return
        }

        Stop-Process -Name $ProviderProcessName -Force -ErrorAction SilentlyContinue
        
        Write-Log "Removing Scheduled Tasks..."
        Manage-ScheduledTask -Remove
        
        Write-Log "Removing from PATH..."
        Manage-Path -Remove
        
        Write-Log "Removing application directory: $UrnetworkHome"
        Remove-Item -Path $UrnetworkHome -Recurse -Force

        if (Test-Path $UserDataDir) {
            $choice = Read-Host "[WARN] Do you want to remove the user data directory ($UserDataDir)? This deletes your auth token. [y/N]"
            if ($choice -eq 'y') {
                Write-Log "Removing user data directory: $UserDataDir"
                Remove-Item -Path $UserDataDir -Recurse -Force
            }
        }
        
        Write-Log "Uninstallation complete. Restart your terminal for PATH changes to take full effect." -Level SUCCESS
    }

    function Invoke-Update {
        Write-Log "Checking for updates..."
        $installedVersion = if (Test-Path $VersionFile) { Get-Content $VersionFile } else { "none" }
        
        $release = Get-GitHubRelease -Tag "latest"
        $latestVersion = $release.tag_name

        if ($installedVersion -eq $latestVersion) {
            Write-Log "You are already using the latest version: $installedVersion" -Level SUCCESS
            return
        }
        
        Write-Log "A new version is available: $latestVersion (You have $installedVersion)"
        Write-Log "Updating..."
        
        # Re-run the install logic with the latest version tag
        Invoke-Install -Version $latestVersion
    }

    function Get-VersionInfo {
        $installedVersion = if (Test-Path $VersionFile) { Get-Content $VersionFile } else { "Not Installed" }
        Write-Host "Urnetwork Manager Script Version : $ScriptVersion"
        Write-Host "Urnetwork Provider Version       : $installedVersion"
    }
}
