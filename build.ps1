<#
.SYNOPSIS
  Build pintu.exe untuk Windows (zip, dan installer kalau diminta).

.EXAMPLE
  .\build.ps1                         # amd64 + arm64, hasil di dist\
  .\build.ps1 -Arch amd64 -Installer  # plus setup.exe (butuh Inno Setup 6)
  .\build.ps1 -Version v1.2.0 -SkipTests
#>
[CmdletBinding()]
param(
    [string]$Version,
    [string[]]$Arch = @('amd64', 'arm64'),
    [switch]$Installer,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

# Native commands do not throw on failure; check the exit code after each one.
function Assert-ExitCode([string]$What) {
    if ($LASTEXITCODE -ne 0) { throw "$What gagal (exit $LASTEXITCODE)" }
}

# Accept "-Arch amd64,arm64" when called from another shell, where it arrives as one string.
$Arch = $Arch | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } | Where-Object { $_ }

if (-not $Version) {
    # PowerShell 5.1 turns redirected stderr into a terminating error under 'Stop'.
    try { $Version = (git describe --tags --always --dirty 2>$null) } catch { $Version = $null }
    if (-not $Version) { $Version = 'dev' }
}
# Windows version resources need four numbers: v1.2.3-4-gabc -> 1.2.3.0
$numeric = '0.0.0.0'
if ($Version -match '^v?(\d+)\.(\d+)\.(\d+)') { $numeric = "$($Matches[1]).$($Matches[2]).$($Matches[3]).0" }
Write-Host "Pintu $Version ($numeric) untuk $($Arch -join ', ')" -ForegroundColor Cyan

if (-not $SkipTests) {
    go vet ./...; Assert-ExitCode 'go vet'
    go test ./...; Assert-ExitCode 'go test'
}

$dist = Join-Path $PSScriptRoot 'dist'
New-Item -ItemType Directory -Force $dist | Out-Null

# Icon + version info -> rsrc_windows_<arch>.syso, linked automatically by go build.
go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json `
    --arch ($Arch -join ',') --product-version $numeric --file-version $numeric
Assert-ExitCode 'go-winres'

$iscc = $null
if ($Installer) {
    $iscc = (Get-Command iscc.exe -ErrorAction SilentlyContinue).Source
    if (-not $iscc) {
        $iscc = @("${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe", "$env:ProgramFiles\Inno Setup 6\ISCC.exe",
            "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe") | Where-Object { Test-Path $_ } | Select-Object -First 1
    }
    if (-not $iscc) { throw 'Inno Setup 6 tidak ditemukan. Pasang dengan: winget install JRSoftware.InnoSetup' }
}

$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    foreach ($a in $Arch) {
        $out = Join-Path $dist "pintu-windows-$a"
        if (Test-Path $out) { Remove-Item -Recurse -Force $out }
        New-Item -ItemType Directory $out | Out-Null

        $env:GOOS = 'windows'; $env:GOARCH = $a; $env:CGO_ENABLED = '0'
        go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "$out\pintu.exe" .
        Assert-ExitCode "go build ($a)"
        Copy-Item .env.example, LICENSE, THIRD_PARTY_NOTICES.md $out

        $zip = Join-Path $dist "pintu-$Version-windows-$a.zip"
        if (Test-Path $zip) { Remove-Item $zip }
        Compress-Archive -Path "$out\*" -DestinationPath $zip
        Write-Host "  $zip" -ForegroundColor Green

        if ($Installer) {
            & $iscc /Q "/DAppVersion=$Version" "/DAppVersionNumeric=$numeric" "/DArch=$a" `
                "/DBinDir=$out" "/O$dist" installer\pintu.iss
            Assert-ExitCode "Inno Setup ($a)"
            Write-Host "  $(Join-Path $dist "pintu-$Version-windows-$a-setup.exe")" -ForegroundColor Green
        }
    }
} finally {
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
}
