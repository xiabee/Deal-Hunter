# Deal-Hunter local CI gate (Windows) — the authoritative acceptance run on the
# office workstation and the laptop. GitHub Actions is not used: hosted quota is
# exhausted, and this script's exit code is the gate.
#
# Usage:  powershell -File scripts\ci-local.ps1 [-Quick]
#Requires -Version 5
param([switch]$Quick)

$ErrorActionPreference = "Stop"
Set-Location -LiteralPath (Join-Path $PSScriptRoot "..")

function Step($name) { Write-Host "`n> $name" -ForegroundColor Cyan }
function Fail($name) { Write-Host "`nX CI FAILED: $name" -ForegroundColor Red; exit 1 }

Step "go version"
go version | Out-Host

$version = "dev"
try { $version = (& git describe --tags --always --dirty 2>$null) } catch {}
$commit = "none"
try { $commit = (& git rev-parse --short HEAD 2>$null) } catch {}
$built = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

Step "gofmt"
$unformatted = gofmt -l cmd, internal
if ($LASTEXITCODE -ne 0) { Fail "gofmt exited $LASTEXITCODE" }
if ($unformatted) {
    Write-Host "run gofmt -w on:"
    $unformatted | Out-Host
    Fail "gofmt needed"
}
Write-Host "all files formatted"

Step "go vet"
go vet ./...
if ($LASTEXITCODE -ne 0) { Fail "go vet" }

Step "build"
New-Item -ItemType Directory -Force -Path dist | Out-Null
$ld = "-s -w -X github.com/xiabee/deal-hunter/internal/version.Version=$version -X github.com/xiabee/deal-hunter/internal/version.Commit=$commit -X github.com/xiabee/deal-hunter/internal/version.BuildDate=$built"
if ($Quick) {
    go build -trimpath -o dist/dealhunter.exe ./cmd/dealhunter
} else {
    go build -trimpath -ldflags $ld -o dist/dealhunter.exe ./cmd/dealhunter
}
if ($LASTEXITCODE -ne 0) { Fail "build" }

Step "unit + integration tests (offline fixtures)"
if ($Quick) { go test ./... } else { go test -race -count=1 ./... }
if ($LASTEXITCODE -ne 0) { Fail "tests" }

Step "secret scan (open-source release gate)"
go run ./cmd/dealhunter secretscan -C .
if ($LASTEXITCODE -ne 0) { Fail "secrets or private topology in the tree" }

Step "offline smoke"
$data = Join-Path ([System.IO.Path]::GetTempPath()) ("dh-ci-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $data | Out-Null
$env:DH_DATA_DIR = $data

$v = & dist/dealhunter.exe version
if ($LASTEXITCODE -ne 0 -or -not ($v -match 'deal-hunter')) { Fail "version smoke" }
Write-Host "  $v"

$s = & dist/dealhunter.exe sources
if ($LASTEXITCODE -ne 0) { Fail "sources exited non-zero" }
foreach ($want in @('openrouter-free-models', 'search-ai-free-cn', 'copilot-plans-snapshot', 'aliyun-benefit')) {
    if (-not ($s -match [regex]::Escape($want))) { Fail "sources smoke: $want missing" }
}
Write-Host "  15 configured sources visible, autonomous collectors present"

$d = & dist/dealhunter.exe doctor -net=false
if ($LASTEXITCODE -ne 0 -or -not ($d -match '体检通过')) { Fail "doctor smoke" }
Write-Host "  doctor passed with no network probes"
Remove-Item Env:DH_DATA_DIR

if (-not $Quick) {
    Step "cross-compile (deploy targets)"
    foreach ($pair in @("linux/amd64", "linux/arm64", "windows/amd64")) {
        $goos, $goarch = $pair -split "/"
        $env:GOOS = $goos; $env:GOARCH = $goarch; $env:CGO_ENABLED = "0"
        go build -trimpath -ldflags "-s -w" -o "dist/dealhunter-$goos-$goarch" ./cmd/dealhunter
        if ($LASTEXITCODE -ne 0) { Fail "cross-build $pair" }
        Write-Host "  ok $pair"
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
    }
}

Write-Host "`nOK CI PASSED  $version ($commit)" -ForegroundColor Green
exit 0
