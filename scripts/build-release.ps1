param(
    [Parameter(Mandatory=$true)][string]$OutputDirectory
)
$ErrorActionPreference = "Stop"
$repo = Split-Path $PSScriptRoot -Parent
Set-Location $repo
if (git status --porcelain) { throw "Commit all source changes before building a deployment release" }
$revision = (git rev-parse HEAD).Trim()
$builtAt = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ")
$stage = Join-Path $OutputDirectory ("receiver-" + $revision.Substring(0,12))
if (Test-Path $stage) { throw "Release staging directory already exists: $stage" }
New-Item -ItemType Directory -Force $OutputDirectory,$stage | Out-Null
Copy-Item -Recurse (Join-Path $repo "deploy") (Join-Path $stage "deploy")
Copy-Item (Join-Path $PSScriptRoot "backup.py"),(Join-Path $PSScriptRoot "deploy.py") $stage
$previousCgo = $env:CGO_ENABLED
$previousOs = $env:GOOS
$previousArch = $env:GOARCH
$previousTmp = $env:GOTMPDIR
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $env:GOTMPDIR = $OutputDirectory
    go build -trimpath -ldflags "-s -w -X main.buildVersion=$revision -X main.buildTime=$builtAt" -o (Join-Path $stage "telemetry-receiver") ./src
    if ($LASTEXITCODE -ne 0) { throw "Go build failed" }
} finally {
    $env:CGO_ENABLED = $previousCgo
    $env:GOOS = $previousOs
    $env:GOARCH = $previousArch
    $env:GOTMPDIR = $previousTmp
}
$binaryHash = (Get-FileHash (Join-Path $stage "telemetry-receiver") -Algorithm SHA256).Hash.ToLowerInvariant()
$metadata = @{ revision=$revision; builtAt=$builtAt; binarySha256=$binaryHash; platform="linux/amd64" }
$metadata | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $stage "BUILD-INFO.json")
$archive = Join-Path $OutputDirectory ("cst-pilot-server-" + $revision.Substring(0,12) + ".tar.gz")
tar -czf $archive -C $stage .
if ($LASTEXITCODE -ne 0) { throw "Release archive failed" }
Write-Output (ConvertTo-Json @{archive=$archive;revision=$revision;binarySha256=$binaryHash})
