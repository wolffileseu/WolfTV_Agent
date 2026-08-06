# One-shot installer for the WolfTV agent autostart task.
#
# Reads wolftv-agent-task.xml from the same directory, patches in the user
# and install-dir placeholders, and registers the task via schtasks. Idempotent:
# re-running replaces the existing task.
#
# Usage (from an elevated PowerShell in the deploy/ directory):
#
#   .\install-autostart.ps1 -InstallDir C:\wolftv -User $env:USERNAME
#
# The InstallDir must contain wolftv-agent.exe and config.json. The account
# named by -User must be the one that logs into the interactive session
# (auto-login enabled, see README "Autostart").

[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$InstallDir,
    [Parameter(Mandatory=$true)][string]$User,
    [string]$TaskName = "WolfTV-Agent"
)

$ErrorActionPreference = "Stop"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$templatePath = Join-Path $scriptDir "wolftv-agent-task.xml"
if (-not (Test-Path $templatePath)) {
    throw "Template not found: $templatePath"
}
if (-not (Test-Path (Join-Path $InstallDir "wolftv-agent.exe"))) {
    throw "wolftv-agent.exe not found under $InstallDir -- copy the binary in first."
}

# Substitute the two per-machine values (username + install directory) into a
# temp copy of the template, then hand it to schtasks. The template ships with
# obvious placeholder values so it doesn't accidentally run on an unconfigured
# box.
$xml = Get-Content -LiteralPath $templatePath -Raw -Encoding UTF8
$xml = $xml -replace "PLACEHOLDER-USERNAME", $User
$xml = $xml -replace "C:\\wolffiles_agent", $InstallDir.TrimEnd("\")

$tmp = New-TemporaryFile
try {
    # schtasks expects UTF-16 LE (BOM) for XML import.
    [System.IO.File]::WriteAllText($tmp.FullName, $xml, [System.Text.UnicodeEncoding]::new($false, $true))
    Write-Host "Registering scheduled task '$TaskName' for user '$User' at '$InstallDir'..."
    & schtasks /Create /TN $TaskName /XML $tmp.FullName /F
    if ($LASTEXITCODE -ne 0) { throw "schtasks failed with code $LASTEXITCODE" }
    Write-Host "Done. Verify with: schtasks /Query /TN $TaskName /V /FO LIST"
    Write-Host "Test-run now with:  schtasks /Run /TN $TaskName"
}
finally {
    Remove-Item -LiteralPath $tmp.FullName -Force -ErrorAction SilentlyContinue
}
