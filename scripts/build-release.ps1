param(
  [Parameter(Mandatory = $true, Position = 0)]
  [string]$Version,
  [Parameter(Mandatory = $true, Position = 1)]
  [string]$OutputDirectory
)

$ErrorActionPreference = 'Stop'
$repository = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if ($Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') {
  throw 'invalid release version'
}
if (Test-Path -LiteralPath $OutputDirectory) {
  throw 'output directory already exists'
}

$output = [IO.Path]::GetFullPath($OutputDirectory)
$asset = 'akastr-agent-linux-amd64'
New-Item -ItemType Directory -Path $output | Out-Null
try {
  $previousCgo = $env:CGO_ENABLED
  $previousGoos = $env:GOOS
  $previousGoarch = $env:GOARCH
  try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    & go -C $repository build -trimpath -ldflags "-s -w -X main.version=$Version" `
      -o (Join-Path $output $asset) ./cmd/akastr-agent
    if ($LASTEXITCODE -ne 0) { throw 'Go release build failed' }
  } finally {
    $env:CGO_ENABLED = $previousCgo
    $env:GOOS = $previousGoos
    $env:GOARCH = $previousGoarch
  }

  $binary = Join-Path $output $asset
  if ((-not (Test-Path -LiteralPath $binary -PathType Leaf)) `
      -or ((Get-Item -LiteralPath $binary).Length -le 0)) {
    throw 'amd64 release binary was not created'
  }
  $binaryStream = [IO.File]::OpenRead($binary)
  $sha256 = [Security.Cryptography.SHA256]::Create()
  try {
    $checksum = [BitConverter]::ToString(
      $sha256.ComputeHash($binaryStream)
    ).Replace('-', '').ToLowerInvariant()
  } finally {
    $sha256.Dispose()
    $binaryStream.Dispose()
  }
  if ($checksum -notmatch '^[0-9a-f]{64}$') {
    throw 'invalid amd64 release binary checksum'
  }
  $template = Get-Content -Raw -LiteralPath (Join-Path $repository 'scripts\install.sh')
  $installer = $template.Replace('@AKASTR_AGENT_VERSION@', $Version).Replace('@AKASTR_AGENT_BINARY_SHA256@', $checksum)
  [IO.File]::WriteAllText(
    (Join-Path $output 'install.sh'),
    $installer.Replace("`r`n", "`n"),
    [Text.UTF8Encoding]::new($false)
  )
  Write-Output "release assets created in $output"
} catch {
  Remove-Item -Recurse -LiteralPath $output
  throw
}
