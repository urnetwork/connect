# Tests for the Windows installer scripts (Provider_Install_Win32.ps1,
# urnet-tools.ps1, urnetwork-updater.ps1, Provider_Uninstall_Win32.ps1).
#
# The helper functions are loaded from each script's AST and tested on their
# own. On Linux, with python3 and cc, the installer and urnet-tools also run
# end to end against a fake release served from 127.0.0.1. The Windows-only
# parts (Startup shortcuts, PATH in the registry) need a real PC; see
# TRY-IT steps in the pull request.
#
# Run: pwsh -NoProfile -File scripts/Provider_Install_Win32.test.ps1
$ErrorActionPreference = "Stop"
$Failures = 0

function Fail([string]$Message) {
    Write-Host "FAIL: $Message"
    $script:Failures++
}

function Check([string]$Name, $Got, $Want) {
    if ("$Got" -ne "$Want") {
        Fail "$Name -> '$Got', want '$Want'"
    }
}

# Get-ScriptFunction returns the named functions' source; dot-source it with
# . ([scriptblock]::Create(...)) where they're needed.
function Get-ScriptFunction([string]$ScriptName, [string[]]$Names) {
    $Path = Join-Path $PSScriptRoot $ScriptName
    $Ast = [System.Management.Automation.Language.Parser]::ParseFile($Path, [ref]$null, [ref]$null)
    $Text = foreach ($Name in $Names) {
        $Definition = $Ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $Name }, $true)
        if (-not $Definition) { throw "$Name is not defined in $ScriptName" }
        $Definition.Extent.Text
    }
    return [scriptblock]::Create($Text -join "`n")
}

# Every script parses in this PowerShell.
foreach ($Name in @("Provider_Install_Win32.ps1", "urnet-tools.ps1", "urnetwork-updater.ps1", "Provider_Uninstall_Win32.ps1")) {
    $Errors = $null
    $null = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot $Name), [ref]$null, [ref]$Errors)
    if ($Errors.Count -gt 0) {
        Fail "$Name has parse errors: $($Errors[0].Message) (line $($Errors[0].Extent.StartLineNumber))"
    }
}

# [Y/n] answers default to yes.
. (Get-ScriptFunction "Provider_Install_Win32.ps1" @("Test-YesAnswer", "Test-SamePathEntry", "Add-PathEntry", "Select-ProviderAsset", "Get-AssetSha256"))
Check "answer ''" (Test-YesAnswer "") $true
Check "answer '   '" (Test-YesAnswer "   ") $true
Check "answer null" (Test-YesAnswer $null) $true
Check "answer y" (Test-YesAnswer "y") $true
Check "answer Y" (Test-YesAnswer "Y") $true
Check "answer yes" (Test-YesAnswer "yes") $true
Check "answer ' Yes '" (Test-YesAnswer " Yes ") $true
Check "answer n" (Test-YesAnswer "n") $false
Check "answer N" (Test-YesAnswer "N") $false
Check "answer no" (Test-YesAnswer "no") $false
Check "answer x" (Test-YesAnswer "x") $false

# Every prompt uses the default-yes helper, not a bare "y" comparison.
$Source = Get-Content -Raw (Join-Path $PSScriptRoot "Provider_Install_Win32.ps1")
if ($Source -match '\.ToLower\(\) -eq "y"') {
    Fail 'a prompt still compares $Answer.ToLower() -eq "y"'
}
$Prompts = ([regex]::Matches($Source, 'Read-Host "[^"]*\[Y/n\]')).Count
$Uses = ([regex]::Matches($Source, 'Test-YesAnswer (\$Answer|\(Read-Host)')).Count
if ($Uses -lt $Prompts) {
    Fail "expected every [Y/n] answer to go through Test-YesAnswer ($Uses of $Prompts)"
}

# No exit outside the final lines: under "irm | iex" it would close the window.
$Lines = $Source -split "`n"
$Exits = @($Lines | Select-String -Pattern '^\s*exit\b')
if ($Exits.Count -ne 1) {
    Fail "the installer should exit only once, at the end, found $($Exits.Count)"
}

# PATH handling.
Check "add to empty PATH" (Add-PathEntry "" 'C:\u\p') 'C:\u\p'
Check "add appends" (Add-PathEntry 'C:\a;C:\b' 'C:\u\p') 'C:\a;C:\b;C:\u\p'
Check "add drops empty entries" (Add-PathEntry 'C:\a;;C:\b;' 'C:\u\p') 'C:\a;C:\b;C:\u\p'
Check "add keeps an existing entry" (Add-PathEntry 'C:\a;c:\U\P\;C:\b' 'C:\u\p') 'C:\a;c:\U\P\;C:\b'
Check "add keeps %VAR% entries" (Add-PathEntry '%USERPROFILE%\bin' 'C:\u\p') '%USERPROFILE%\bin;C:\u\p'
Check "same entry ignores case and trailing slash" (Test-SamePathEntry 'C:\X\' 'c:\x') $true
Check "different entries" (Test-SamePathEntry 'C:\X' 'C:\Y') $false

# Release asset choice and digest.
$AllPlatform = [pscustomobject]@{ name = "urnetwork-provider-2026.10.20-1.tar.gz"; digest = "sha256:" + ("AB" * 32) }
$WinArm = [pscustomobject]@{ name = "urnetwork-provider-2026.10.20-1-windows-arm64.tar.gz"; digest = "" }
$Other = [pscustomobject]@{ name = "urnetwork-proxy-socks-2026.10.20-1.tar.gz" }
$Assets = @($Other, $AllPlatform, $WinArm)
Check "asset prefers platform file" (Select-ProviderAsset $Assets "windows" "arm64").name $WinArm.name
Check "asset falls back to all-platform file" (Select-ProviderAsset $Assets "windows" "amd64").name $AllPlatform.name
Check "asset none" (Select-ProviderAsset @($Other) "windows" "amd64") ""
Check "digest" (Get-AssetSha256 $AllPlatform) ("ab" * 32)
Check "no digest" (Get-AssetSha256 $WinArm) ""
Check "bad digest" (Get-AssetSha256 ([pscustomobject]@{ digest = "sha512:00" })) ""

# urnet-tools helpers.
. (Get-ScriptFunction "urnet-tools.ps1" @("ConvertTo-Interval", "Remove-PathEntry"))
Check "interval daily" (ConvertTo-Interval "daily") "day"
Check "interval every-week" (ConvertTo-Interval "every-week") "week"
Check "interval Month" (ConvertTo-Interval " Month ") "month"
Check "interval bad" (ConvertTo-Interval "hourly") ""
Check "remove entry" (Remove-PathEntry 'C:\a;C:\U\P\;;C:\b' 'c:\u\p') 'C:\a;C:\b'
Check "remove missing entry" (Remove-PathEntry 'C:\a' 'c:\u\p') 'C:\a'

# The uninstaller also removes the older windows\<arch> PATH entries.
& {
    . (Get-ScriptFunction "Provider_Uninstall_Win32.ps1" @("Test-SamePathEntry", "Remove-PathEntry"))
    Check "uninstall removes old layout" (Remove-PathEntry 'C:\a;C:\p\windows\amd64;C:\p' @('C:\p', 'C:\p\windows\amd64')) 'C:\a'
}

# End to end, on Linux only.
function Invoke-EndToEnd {
    $Python = Get-Command python3 -ErrorAction SilentlyContinue
    $Cc = Get-Command cc -ErrorAction SilentlyContinue
    if (-not $IsLinux -or -not $Python -or -not $Cc) {
        Write-Host "SKIP: end-to-end test (needs Linux, python3 and cc)"
        return
    }

    $Root = Join-Path ([IO.Path]::GetTempPath()) "urnetwork-installer-test-$PID"
    $Serve = Join-Path $Root "serve"
    $Install = Join-Path $Root "install"
    $State = Join-Path $Root "state"
    $null = New-Item -ItemType Directory -Force -Path (Join-Path $Serve "api/releases/tags"), (Join-Path $Serve "scripts"), $State
    Copy-Item (Join-Path $PSScriptRoot "*.ps1") (Join-Path $Serve "scripts")

    $Arch = switch ("$(uname -m)".Trim()) { "x86_64" { "amd64" } default { "arm64" } }

    # A fake provider: "provide" runs until stopped, "auth" signs in.
    $Source = Join-Path $Root "fake.c"
    Set-Content $Source @'
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>
int main(int argc, char **argv) {
    if (argc > 2 && strcmp(argv[2], "--help") == 0) { puts("provide [--allow-client-registration]"); return 0; }
    if (argc > 1 && strcmp(argv[1], "provide") == 0) {
        /* Like the released miner: a new device needs --allow-client-registration. */
        char path[4096]; snprintf(path, sizeof path, "%s/.provider.jwt", getenv("URNETWORK_STATE_DIR"));
        FILE *f = fopen(path, "r");
        if (f) { fclose(f); }
        else if (getenv("FAKE_REJECT")) { puts("network_sign_in_rejected"); fflush(stdout); for (;;) sleep(60); }
        else if (argc > 2 && strcmp(argv[2], "--allow-client-registration") == 0 && !getenv("FAKE_CANT_REGISTER")) {
            f = fopen(path, "w"); if (!f) return 1; fputs("x", f); fclose(f);
            snprintf(path, sizeof path, "%s/.provider.key", getenv("URNETWORK_STATE_DIR"));
            f = fopen(path, "w"); if (!f) return 1; fputs("x", f); fclose(f);
        }
        else { puts("startup_recovery_required"); fflush(stdout); return 0; }
        for (;;) sleep(60);
    }
    if (argc > 1 && strcmp(argv[1], "update") == 0) { for (;;) sleep(60); }
    if (argc > 1 && strcmp(argv[1], "auth") == 0) {
        char path[4096]; snprintf(path, sizeof path, "%s/jwt", getenv("URNETWORK_STATE_DIR"));
        FILE *f = fopen(path, "w"); if (!f) return 1; fputs("x", f); fclose(f); return 0;
    }
    puts(VERSION); return 0;
}
'@

    function New-Release([string]$Tag, [string]$Digest = "") {
        $Build = Join-Path $Root "build-$Tag"
        $null = New-Item -ItemType Directory -Force -Path (Join-Path $Build "linux/$Arch")
        & cc "-DVERSION=`"$Tag`"" -o (Join-Path $Build "linux/$Arch/provider") $Source
        $Archive = "urnetwork-provider-$Tag.tar.gz"
        & tar -czf (Join-Path $Serve $Archive) -C $Build linux
        if (-not $Digest) {
            $Digest = (Get-FileHash (Join-Path $Serve $Archive) -Algorithm SHA256).Hash.ToLower()
        }
        $Release = @{
            tag_name = $Tag
            published_at = "2026-10-21T02:44:41Z"
            assets = @(@{ name = $Archive; digest = "sha256:$Digest"; browser_download_url = "$BaseUrl/$Archive" })
        } | ConvertTo-Json -Depth 5
        Set-Content (Join-Path $Serve "api/releases/latest") $Release
        Set-Content (Join-Path $Serve "api/releases/tags/$Tag") $Release
    }

    $Port = Get-Random -Minimum 20000 -Maximum 60000
    $BaseUrl = "http://127.0.0.1:$Port"
    $ServerScript = Join-Path $Root "server.py"
    Set-Content $ServerScript @'
import http.server, sys, os
os.chdir(sys.argv[2])
class H(http.server.SimpleHTTPRequestHandler):
    def guess_type(self, path):
        return "application/json" if "/api/" in path else "application/octet-stream"
    def log_message(self, *a):
        pass
http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
'@
    $Server = Start-Process -FilePath $Python.Source -ArgumentList "-I", $ServerScript, $Port, $Serve -PassThru
    $env:URNETWORK_STATE_DIR = $State
    $env:URNETWORK_RELEASE_API_URL = "$BaseUrl/api"
    $Pwsh = [Diagnostics.Process]::GetCurrentProcess().MainModule.FileName
    $Installer = Join-Path $PSScriptRoot "Provider_Install_Win32.ps1"
    $Tools = Join-Path $Install "urnet-tools.ps1"

    try {
        Start-Sleep -Seconds 1
        New-Release "v2026.10.20-1"

        $Out = & $Pwsh -NoProfile -File $Installer -Destination $Install -NonInteractive -ScriptsBaseUrl "$BaseUrl/scripts" 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0) { Fail "install failed: $Out" }
        Check "installed version" (Get-Content (Join-Path $Install "version")) "v2026.10.20-1"
        Check "installed binary runs" (& (Join-Path $Install "urnetwork") --version) "v2026.10.20-1"
        Check "scripts-url saved" (Get-Content (Join-Path $Install "scripts-url")) "$BaseUrl/scripts"
        Check "date is invariant" (Get-Content (Join-Path $Install "date")) "2026-10-21T02:44:41Z"
        if ($Out -notmatch "Checked the SHA-256") { Fail "install didn't check the digest: $Out" }
        if (Test-Path ([IO.Path]::Combine([IO.Path]::GetTempPath(), "urnetwork-install", "extracted"))) { Fail "temporary files left behind" }

        # A damaged download changes nothing.
        New-Release "v2026.10.20-2" ("00" * 32)
        $Out = & $Pwsh -NoProfile -File $Installer -Destination $Install -NonInteractive 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0) { Fail "install with a bad digest succeeded" }
        if ($Out -notmatch "damaged") { Fail "bad digest message: $Out" }
        Check "bad digest keeps version" (Get-Content (Join-Path $Install "version")) "v2026.10.20-1"

        # A version that doesn't exist.
        $Out = & $Pwsh -NoProfile -File $Installer -Destination $Install -NonInteractive -Version "nope" 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "Couldn't get release 'nope'") { Fail "missing version: $Out" }
        $Out = & $Pwsh -NoProfile -File $Installer -Version "a/b" 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "slash") { Fail "slash in version accepted: $Out" }

        # Status, start, stop.
        $Out = & $Pwsh -NoProfile -File $Tools status 2>&1 | Out-String
        if ($LASTEXITCODE -ne 3 -or $Out -notmatch "Status: stopped" -or $Out -notmatch "Signed in: no") { Fail "status before start (exit $LASTEXITCODE): $Out" }
        $Out = & $Pwsh -NoProfile -File $Tools start 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "not signed in") { Fail "start before sign-in: $Out" }
        & (Join-Path $Install "urnetwork") auth
        # Started by hand, a new device stops: it needs registering.
        $Plain = Start-Process -FilePath (Join-Path $Install "urnetwork") -ArgumentList "provide" -PassThru -RedirectStandardOutput (Join-Path $Root "plain.out")
        $Plain.WaitForExit(5000) | Out-Null
        if (-not $Plain.HasExited) { Fail "fake provider should stop without registration" }
        # A release that can't register says so, not "restore your backup".
        $env:FAKE_CANT_REGISTER = "1"
        $Out = & $Pwsh -NoProfile -File $Tools start 2>&1 | Out-String
        Remove-Item Env:FAKE_CANT_REGISTER
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "Registering this device failed") { Fail "start that can't register: $Out" }
        # A refused sign-in stops the registering miner at once, with an auth hint.
        $env:FAKE_REJECT = "1"
        $Out = & $Pwsh -NoProfile -File $Tools start 2>&1 | Out-String
        Remove-Item Env:FAKE_REJECT
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "refused" -or $Out -notmatch "urnetwork auth") { Fail "refused registration: $Out" }
        if (@(Get-Process -Name urnetwork -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Install/*" }).Count -ne 0) { Fail "refused registration left the miner running" }
        $Out = & $Pwsh -NoProfile -File $Tools start 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0 -or $Out -notmatch "running in the background" -or $Out -notmatch "This device is registered") { Fail "first start: $Out" }
        if (-not (Test-Path (Join-Path $State ".provider.jwt"))) { Fail "first start didn't register the device" }
        $Out = & $Pwsh -NoProfile -File $Tools restart 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0 -or $Out -match "registering") { Fail "a registered device registered again: $Out" }
        Set-Content (Join-Path $State ".provider.jwt.rejected") "x"
        & $Pwsh -NoProfile -File $Tools stop | Out-Null
        $Out = & $Pwsh -NoProfile -File $Tools start 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "removed from your URnetwork account") { Fail "revoked device: $Out" }
        Remove-Item -Force (Join-Path $State ".provider.jwt.rejected")
        $Out = & $Pwsh -NoProfile -File $Tools start 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0) { Fail "start after revoked test: $Out" }
        $Cmd = @(Get-Process -Name urnetwork -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Install/*" }).CommandLine
        if ("$Cmd" -match "allow-client-registration") { Fail "restart still asks to register: $Cmd" }
        $Out = & $Pwsh -NoProfile -File $Tools start 2>&1 | Out-String
        if ($Out -notmatch "already running") { Fail "second start: $Out" }
        $Out = & $Pwsh -NoProfile -File $Tools status 2>&1 | Out-String
        if ($Out -notmatch "Device registered: yes") { Fail "status registration: $Out" }
        $Out = & $Pwsh -NoProfile -File $Tools status 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0 -or $Out -notmatch "Status: running" -or $Out -notmatch "Signed in: yes") { Fail "status after start: $Out" }

        # An update replaces the running provider and restarts it.
        New-Release "v2026.10.21-1"
        # Stands in for the miner's "provider update" handing off to urnet-tools:
        # the update must not kill it.
        $Caller = Start-Process -FilePath (Join-Path $Install "urnetwork") -ArgumentList "update" -PassThru
        $Before = @(Get-Process -Name urnetwork -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Install/*" }).Id
        $Out = & $Pwsh -NoProfile -File $Tools update 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0) { Fail "update failed: $Out" }
        Check "updated version" (Get-Content (Join-Path $Install "version")) "v2026.10.21-1"
        Check "updated binary" (& (Join-Path $Install "urnetwork") --version) "v2026.10.21-1"
        if (-not (Test-Path (Join-Path $Install "provider.log"))) { Fail "no provider.log" }
        if ($Caller.HasExited) { Fail "the update killed the urnetwork process that isn't a provider" }
        Stop-Process -Id $Caller.Id -Force -ErrorAction SilentlyContinue
        Start-Sleep -Milliseconds 300
        $After = @(Get-Process -Name urnetwork -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Install/*" }).Id
        if ($After.Count -ne 1 -or ($Before -contains $After[0])) { Fail "provider wasn't restarted on the new version (before $Before, after $After)" }
        $Out = & $Pwsh -NoProfile -File $Tools update 2>&1 | Out-String
        if ($Out -notmatch "up to date") { Fail "second update: $Out" }

        # Reinstall keeps the identity.
        $Out = & $Pwsh -NoProfile -File $Tools reinstall 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0) { Fail "reinstall failed: $Out" }
        if (-not (Test-Path (Join-Path $State "jwt"))) { Fail "reinstall deleted the identity" }

        # Uninstall keeps the identity, then -RemoveData deletes it.
        $Out = & $Pwsh -NoProfile -File $Tools uninstall 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0) { Fail "uninstall failed: $Out" }
        if (Test-Path $Install) { Fail "uninstall left ${Install}: $(Get-ChildItem $Install)" }
        if (-not (Test-Path (Join-Path $State "jwt"))) { Fail "uninstall deleted the identity" }
        if (@(Get-Process -Name urnetwork -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Install/*" }).Count -ne 0) { Fail "uninstall left the provider running" }

        Remove-Item (Join-Path $State "jwt")
        $Out = & $Pwsh -NoProfile -File $Installer -Destination $Install -NonInteractive -t "2026.10.21-1" -AuthCode "code" -ScriptsBaseUrl "$BaseUrl/scripts" 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path (Join-Path $State "jwt"))) { Fail "install with -AuthCode and a tag without v: $Out" }
        $Out = & $Pwsh -NoProfile -File $Installer -Destination $Install -NonInteractive 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0 -or $Out -notmatch "Already signed in") { Fail "install over a kept identity: $Out" }
        $Out = & $Pwsh -NoProfile -File $Tools uninstall -RemoveIdentity 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "Nothing was removed" -or -not (Test-Path (Join-Path $Install "urnetwork"))) { Fail "uninstall -RemoveIdentity without -Yes and no terminal: $Out" }
        $Out = & $Pwsh -NoProfile -File $Tools uninstall --remove-identity 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "Nothing was removed") { Fail "uninstall --remove-identity without --yes: $Out" }
        $Out = & $Pwsh -NoProfile -File $Tools uninstall --bogus 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0 -or $Out -notmatch "Unknown option") { Fail "unknown option: $Out" }
        $Out = & $Pwsh -NoProfile -File $Tools uninstall --remove-identity --yes 2>&1 | Out-String
        if (Test-Path $State) { Fail "uninstall -RemoveData kept ${State}: $Out" }

        # Under "irm | iex" a failure must not end the caller's session.
        $Out = & $Pwsh -NoProfile -Command "`$env:URNETWORK_RELEASE_API_URL = '$BaseUrl/missing'; Get-Content -Raw '$Installer' | Invoke-Expression; 'still here'" 2>&1 | Out-String
        if ($Out -notmatch "still here") { Fail "iex failure closed the session: $Out" }
    }
    finally {
        Get-Process -Name urnetwork -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Root/*" } | Stop-Process -Force
        Stop-Process -Id $Server.Id -Force -ErrorAction SilentlyContinue
        Remove-Item -Recurse -Force $Root -ErrorAction SilentlyContinue
        Remove-Item Env:URNETWORK_STATE_DIR, Env:URNETWORK_RELEASE_API_URL -ErrorAction SilentlyContinue
    }
}
Invoke-EndToEnd

if ($Failures -gt 0) { Write-Host "Provider_Install_Win32: $Failures failure(s)"; exit 1 }
Write-Host "Provider_Install_Win32: OK"
