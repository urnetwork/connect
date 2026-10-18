# Tests the [Y/n] answer handling in Provider_Install_Win32.ps1 without running
# the installer: the Test-YesAnswer function is loaded from the script's AST.
# Run: pwsh -NoProfile -File scripts/Provider_Install_Win32.test.ps1
$ErrorActionPreference = "Stop"
$ScriptPath = Join-Path $PSScriptRoot "Provider_Install_Win32.ps1"
$Ast = [System.Management.Automation.Language.Parser]::ParseFile($ScriptPath, [ref]$null, [ref]$null)
$Definition = $Ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq "Test-YesAnswer" }, $true)
if (-not $Definition) { throw "Test-YesAnswer is not defined in $ScriptPath" }
. ([scriptblock]::Create($Definition.Extent.Text))

$Failures = 0
function Check([string]$Answer, [bool]$Want) {
    $Got = Test-YesAnswer $Answer
    if ($Got -ne $Want) {
        Write-Host "FAIL: answer '$Answer' -> $Got, want $Want"
        $script:Failures++
    }
}
Check "" $true        # plain Enter takes the default: yes
Check "   " $true
Check $null $true
Check "y" $true
Check "Y" $true
Check "yes" $true
Check " Yes " $true
Check "n" $false
Check "N" $false
Check "no" $false
Check "x" $false

# Both prompts must use the default-yes helper, not a bare "y" comparison.
$Source = Get-Content -Raw $ScriptPath
if ($Source -match '\.ToLower\(\) -eq "y"') {
    Write-Host 'FAIL: a prompt still compares $Answer.ToLower() -eq "y"'
    $Failures++
}
$Uses = ([regex]::Matches($Source, 'if \(Test-YesAnswer \$Answer\)')).Count
if ($Uses -lt 2) {
    Write-Host "FAIL: expected both prompts to use Test-YesAnswer, found $Uses"
    $Failures++
}

if ($Failures -gt 0) { Write-Host "Provider_Install_Win32: $Failures failure(s)"; exit 1 }
Write-Host "Provider_Install_Win32: OK"
