param(
    [string]$InstallDir = "$env:USERPROFILE\.local\bin"
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$binaryDir = Join-Path $repoRoot "bin"
$sourceBinary = Join-Path $binaryDir "taskpilot.exe"
$targetBinary = Join-Path $InstallDir "taskpilot.exe"

Write-Host "Building TaskPilot..."
Push-Location $repoRoot
try {
    New-Item -ItemType Directory -Force $binaryDir | Out-Null
    go build -o $sourceBinary .\cmd\taskpilot
}
finally {
    Pop-Location
}

Write-Host "Installing TaskPilot to $InstallDir..."
New-Item -ItemType Directory -Force $InstallDir | Out-Null
Copy-Item $sourceBinary $targetBinary -Force

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$paths = @()
if ($userPath) {
    $paths = $userPath -split ';' | Where-Object { $_ -ne "" }
}

$alreadyOnPath = $paths | Where-Object { $_.TrimEnd('\') -ieq $InstallDir.TrimEnd('\') }
if (-not $alreadyOnPath) {
    $newPath = @($InstallDir) + $paths
    [Environment]::SetEnvironmentVariable("Path", ($newPath -join ';'), "User")
    $env:Path = "$InstallDir;$env:Path"
    Write-Host "Added $InstallDir to your User PATH."
    Write-Host "Open a new PowerShell window if 'taskpilot' is not found immediately."
}

Write-Host "Installed taskpilot.exe:"
Get-Item $targetBinary | Format-List FullName, Length, LastWriteTime
Write-Host "Verify with:"
Write-Host "  taskpilot config show"
