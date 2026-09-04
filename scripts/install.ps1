# Install MySQL Tester on Windows.
#
#   irm https://raw.githubusercontent.com/cubetiqlabs/cubisoft-tester/main/scripts/install.ps1 | iex
#
# Params: -Version 1.2.3 to pin a version, -InstallDir to change the target.
param(
    [string]$Version = "",
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\MySQLTester"
)
$ErrorActionPreference = "Stop"

$repo = "cubetiqlabs/cubisoft-tester"
$app  = "mysqltester"
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }

if (-not $Version) {
    $Version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name -replace '^v', ''
}

$asset = "${app}_${Version}_windows_${arch}.zip"
$base  = "https://github.com/$repo/releases/download/v$Version"
$tmp   = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    Write-Host "Downloading $asset"
    Invoke-WebRequest "$base/$asset" -OutFile "$tmp\$asset" -UseBasicParsing

    # The release publishes checksums.txt; refuse to install a mismatched download.
    try {
        Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt" -UseBasicParsing
        $want = (Select-String -Path "$tmp\checksums.txt" -Pattern "\s$([regex]::Escape($asset))$").Line -split '\s+' | Select-Object -First 1
        if ($want) {
            $got = (Get-FileHash "$tmp\$asset" -Algorithm SHA256).Hash.ToLower()
            if ($want.ToLower() -ne $got) { throw "checksum mismatch for $asset" }
            Write-Host "Checksum verified"
        }
    } catch [System.Net.WebException] { }

    Expand-Archive "$tmp\$asset" -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item "$tmp\$app.exe" (Join-Path $InstallDir "$app.exe") -Force

    $lnk = Join-Path ([Environment]::GetFolderPath("Programs")) "MySQL Tester.lnk"
    $s = (New-Object -ComObject WScript.Shell).CreateShortcut($lnk)
    $s.TargetPath = Join-Path $InstallDir "$app.exe"
    $s.Save()

    Write-Host "Installed $InstallDir\$app.exe"
    Write-Host "Start menu shortcut: MySQL Tester"
} finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
