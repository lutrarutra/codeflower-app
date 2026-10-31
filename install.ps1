# Installs the code flower app on Windows from its latest GitHub release, in PowerShell:
#
#   irm https://raw.githubusercontent.com/lutrarutra/codeflower-app/main/install.ps1 | iex
#
# It puts codeflower.exe on your PATH and code flower (the window) in the Start menu.
# $env:CODEFLOWER_VERSION = "v0.1.0" installs that release instead, and
# $env:CODEFLOWER_INSTALL_DIR chooses the folder (default %LOCALAPPDATA%\Programs\codeflower).
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$repo = "lutrarutra/codeflower-app"
$version = if ($env:CODEFLOWER_VERSION) { $env:CODEFLOWER_VERSION } else { "latest" }
$directory = if ($env:CODEFLOWER_INSTALL_DIR) { $env:CODEFLOWER_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\codeflower" }

function Fail($message) {
  # throw, not exit: piped into iex, exit would close the user's PowerShell.
  throw "codeflower install: $message"
}

@(
  "NOTICE: code flower runs autonomous AI agents that may make decisions, run commands,"
  "and continue working without constant supervision. Their use may consume substantial"
  "model tokens and may incur charges under your provider's plan or API pricing."
  ""
  "Sandboxing is intended to limit access, not guarantee safety. Agents can access data"
  "you mount; read-write mounts may be changed or deleted. Incorrect or overly broad"
  "mounts may expose or damage data. Carefully review mounts, permissions, and network"
  "settings before each run, and keep backups of important data."
  ""
  "You are responsible for your instructions, configuration, mount choices, supervision,"
  "and review of agent actions and results. Do not mount data you are not authorized to"
  "share. Use of this software is at your own risk. To the extent permitted by law, the"
  "software is provided as-is, without warranties, and its developers disclaim liability"
  "for loss, costs, or damages arising from its use."
  ""
  "To continue installation, type y and press Enter. Any other response cancels."
) | ForEach-Object { Write-Host $_ }
if ((Read-Host) -cne "y") { Fail "installation cancelled" }

# The only Windows build; Windows on Arm runs it too.
$platform = "windows-amd64"
$work = Join-Path ([IO.Path]::GetTempPath()) ("codeflower-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null
try {
  # The archives are named after their release, so "latest" is looked up first.
  if ($version -eq "latest") {
    $version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
    if (-not $version) { Fail "could not read the latest release's version" }
  }
  $base = "https://github.com/$repo/releases/download/$version"
  $archive = "codeflower-$version-$platform.zip"

  Write-Host "Downloading $archive..."
  try {
    Invoke-WebRequest "$base/$archive" -OutFile (Join-Path $work $archive) -UseBasicParsing
  } catch {
    # Releases up to v0.10.0 named their archives without the version.
    $archive = "codeflower-$platform.zip"
    try { Invoke-WebRequest "$base/$archive" -OutFile (Join-Path $work $archive) -UseBasicParsing }
    catch { Fail "could not download $base/$archive" }
  }
  try { Invoke-WebRequest "$base/SHA256SUMS" -OutFile (Join-Path $work "SHA256SUMS") -UseBasicParsing }
  catch { Fail "could not download the checksums" }

  $expected = Get-Content (Join-Path $work "SHA256SUMS") |
    ForEach-Object { $fields = $_ -split "\s+"; if ($fields[1] -ceq $archive) { $fields[0] } } |
    Select-Object -First 1
  if (-not $expected) { Fail "$archive is not in SHA256SUMS" }
  $actual = (Get-FileHash (Join-Path $work $archive) -Algorithm SHA256).Hash
  if ($actual -ne $expected) { Fail "checksum mismatch for $archive" }

  Expand-Archive (Join-Path $work $archive) -DestinationPath (Join-Path $work "archive")
  New-Item -ItemType Directory -Force -Path $directory | Out-Null
  $executable = Join-Path $directory "codeflower.exe"
  # A running program cannot be replaced, only moved aside, as codeflower --update does.
  Remove-Item "$executable.old" -Force -ErrorAction SilentlyContinue
  if (Test-Path $executable) { Move-Item $executable "$executable.old" -Force }
  Move-Item (Join-Path $work "archive\codeflower.exe") $executable
  # Downloaded with the browser's mark, Windows would ask before each start.
  Unblock-File $executable
  Write-Host "Installed $(& $executable --version) in $directory"

  # Shown in the Start menu as code flower; it opens the window, with the
  # icon built into the program (none in releases up to v0.11.0).
  $programs = [Environment]::GetFolderPath("Programs")
  $shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut((Join-Path $programs "code flower.lnk"))
  $shortcut.TargetPath = $executable
  $shortcut.Arguments = "client --desktop"
  $shortcut.WorkingDirectory = $env:USERPROFILE
  $shortcut.IconLocation = "$executable,0"
  $shortcut.Description = "Start code flower sandboxes and connect to them"
  $shortcut.Save()
  Write-Host "Added code flower to the Start menu"

  $path = [Environment]::GetEnvironmentVariable("Path", "User")
  if (-not (($path -split ";") -contains $directory)) {
    [Environment]::SetEnvironmentVariable("Path", (@($path, $directory) | Where-Object { $_ }) -join ";", "User")
    Write-Host "Added $directory to your PATH; open a new terminal to use it."
  }
  Write-Host ""
  Write-Host "Next: codeflower check"
} finally {
  Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
}
