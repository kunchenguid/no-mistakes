$ErrorActionPreference = "Stop"

$repo = "kunchenguid/no-mistakes"
$installDir = "$env:LOCALAPPDATA\no-mistakes"
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }

$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest"
$version = $release.tag_name
if (-not $version) {
    throw "Could not determine latest release"
}

$filename = "no-mistakes-$version-windows-$arch.zip"
$url = "https://github.com/$repo/releases/download/$version/$filename"

$tmpDir = New-TemporaryFile | ForEach-Object {
    Remove-Item $_
    New-Item -ItemType Directory -Path $_
}

Write-Host "Downloading no-mistakes $version for windows/$arch..."
Invoke-WebRequest -Uri $url -OutFile "$tmpDir\$filename"
Expand-Archive -Path "$tmpDir\$filename" -DestinationPath $tmpDir -Force

New-Item -ItemType Directory -Path $installDir -Force | Out-Null
Move-Item -Path "$tmpDir\no-mistakes.exe" -Destination "$installDir\no-mistakes.exe" -Force
Remove-Item -Recurse -Force $tmpDir

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$installDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
    Write-Host "Added $installDir to user PATH. Restart your terminal."
}

$restart = Start-Process -FilePath "$installDir\no-mistakes.exe" -ArgumentList @(
    "daemon",
    "restart"
) -Wait -PassThru -NoNewWindow
if ($restart.ExitCode -ne 0) {
    throw "Failed to restart daemon (exit code $($restart.ExitCode))"
}

Write-Host "no-mistakes $version installed to $installDir\no-mistakes.exe"
