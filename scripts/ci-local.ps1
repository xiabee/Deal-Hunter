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

# Mirror of the bash gate: every httpapi test drives the handler through httptest,
# so nothing but this step runs Serve() - the bind, the enabled gate, the public-bind
# rule and the -hold shutdown path. Native commands write stderr on purpose, which
# Stop would turn into a terminating error, so the preference is lowered here only.
Step "served panel: the real listener answers, then exits on its own"
$serveDir = Join-Path ([System.IO.Path]::GetTempPath()) ("dh-serve-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $serveDir | Out-Null
$serveLog = Join-Path $serveDir "serve.log"
$serveCfg = Join-Path $serveDir "config.json"
$publicCfg = Join-Path $serveDir "public.json"
Set-Content -LiteralPath $serveCfg -Value '{"sources":[],"server":{"enabled":true,"bind":"127.0.0.1:0"}}' -Encoding ascii
Set-Content -LiteralPath $publicCfg -Value '{"sources":[],"server":{"enabled":true,"bind":"0.0.0.0:0"}}' -Encoding ascii
$serveExe = Join-Path (Get-Location) "dist/dealhunter.exe"

$ErrorActionPreference = "Continue"
$env:DH_DATA_DIR = $serveDir
$serveProc = Start-Process -FilePath $serveExe `
    -ArgumentList @("-config", $serveCfg, "once", "-serve", "-hold", "20s") `
    -RedirectStandardOutput $serveLog -RedirectStandardError (Join-Path $serveDir "serve.err") `
    -PassThru -NoNewWindow -ErrorAction SilentlyContinue
if (-not $serveProc) {
    $ErrorActionPreference = "Stop"
    Remove-Item -Recurse -Force $serveDir
    Fail "could not start $serveExe (built yet? the build step writes dist/dealhunter.exe)"
}
$serveUrl = ""
for ($try = 0; $try -lt 200; $try++) {
    $hit = Select-String -Path $serveLog -Pattern 'url=http://127\.0\.0\.1:\d+' | Select-Object -First 1
    if ($hit) { $serveUrl = $hit.Matches[0].Value.Substring(4); break }
    if ($serveProc.HasExited) { break }
    Start-Sleep -Milliseconds 100
}
if (-not $serveUrl) {
    Get-Content $serveLog | Out-Host
    Stop-Process -Id $serveProc.Id -Force -ErrorAction SilentlyContinue
    $ErrorActionPreference = "Stop"
    Remove-Item -Recurse -Force $serveDir
    Fail "the panel never reported a listening URL"
}
Write-Host "  listening on $serveUrl"

foreach ($path in @('/healthz', '/', '/api/v1/status', '/api/v1/sources')) {
    $code = (& curl.exe -s -o NUL -w "%{http_code}" "$serveUrl$path")
    if ($code -notmatch '^\d+$') { $ErrorActionPreference = "Stop"; Fail "GET $path returned no HTTP status at all" }
    if ($code -ne "200") { $ErrorActionPreference = "Stop"; Fail "GET $path over the real listener = $code, want 200" }
}
Write-Host "  healthz, panel, status and sources all answered 200"

$panelHtml = ((& curl.exe -sS "$serveUrl/") -join "`n")
$themeHits = ([regex]::Matches($panelHtml, 'data-theme')).Count
if ($themeHits -lt 1) { $ErrorActionPreference = "Stop"; Fail "the served panel lost its theme switch" }
$statusJson = ((& curl.exe -sS "$serveUrl/api/v1/status") -join "")
if ($statusJson -notmatch [regex]::Escape('"live_in_briefing"')) {
    $ErrorActionPreference = "Stop"; Fail "status payload lost live_in_briefing"
}
Write-Host "  panel HTML carries the theme switch ($themeHits hits) and status carries the briefing count"

$env:DH_DATA_DIR = $serveDir
$publicOut = (& $serveExe -config $publicCfg once -serve -hold 2s 2>&1)
$publicRc = $LASTEXITCODE
Remove-Item Env:DH_DATA_DIR
if ($publicRc -eq 0) { $ErrorActionPreference = "Stop"; Fail "a public bind started without complaint" }
if (($publicOut -join "`n") -match 'listening') {
    $ErrorActionPreference = "Stop"; Fail "a public bind reached the listener despite the guard"
}
Write-Host "  a public bind is refused before anything listens"

# Start-Process -PassThru hands back an empty ExitCode on this host (verified: only
# -Wait populates it, and -Wait cannot run while we probe). So the exit-code half of
# "it shuts down cleanly" is asserted by the bash twin; here we assert the observable
# half - it stopped answering once -hold expired, and nothing logged an error.
$exited = $serveProc.WaitForExit(30000)
if (-not $exited) {
    $ErrorActionPreference = "Stop"
    Stop-Process -Id $serveProc.Id -Force -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $serveDir
    Fail "still serving 30s after -hold 20s expired"
}
$afterCode = (& curl.exe -s -o NUL -w "%{http_code}" --max-time 3 "$serveUrl/healthz")
if ($afterCode -match '^2') {
    $ErrorActionPreference = "Stop"
    Remove-Item -Recurse -Force $serveDir
    Fail "the listener still answers after the hold expired"
}
if (Select-String -Path $serveLog -Pattern 'level=ERROR' -Quiet) {
    $ErrorActionPreference = "Stop"
    Remove-Item -Recurse -Force $serveDir
    Fail "the served round logged an error"
}
$ErrorActionPreference = "Stop"
Remove-Item -Recurse -Force $serveDir
Write-Host "  stopped serving when -hold expired, with no ERROR in the log"

# Same step as the bash gate. The drill itself exits 0 with a visible "skipped"
# message when the host cannot set POSIX modes, so running it here is informative
# either way - but only if bash is on PATH at all.
Step "backup drill: sandbox restore, retention and refusals"
if (Get-Command bash -ErrorAction SilentlyContinue) {
    & bash scripts/test-backup.sh
    if ($LASTEXITCODE -ne 0) { Fail "backup drill" }
} else {
    Write-Host "  ! 本机没有 bash，跳过（Linux 侧由 ci-office.sh 执行同一条）" -ForegroundColor Yellow
}

# Same as the bash gate. The drill prints a visible "skipped" under MINGW (its
# failure legs are built from POSIX permission bits), so this step is informative
# rather than red on Windows.
Step "openclaw installer drill: one previous copy, refusal when it cannot be written"
if (Get-Command bash -ErrorAction SilentlyContinue) {
    & bash scripts/test-openclaw-integration.sh
    if ($LASTEXITCODE -ne 0) { Fail "openclaw installer drill" }
} else {
    Write-Host "  ! 本机没有 bash，跳过（Linux 侧由 ci-office.sh 执行同一条）" -ForegroundColor Yellow
}

if (-not $Quick) {
    Step "cross-compile (deploy targets)"
    foreach ($pair in @("linux/amd64", "linux/arm64", "windows/amd64")) {
        $goos, $goarch = $pair -split "/"
        $env:GOOS = $goos; $env:GOARCH = $goarch; $env:CGO_ENABLED = "0"
        go build -trimpath -ldflags $ld -o "dist/dealhunter-$goos-$goarch" ./cmd/dealhunter
        if ($LASTEXITCODE -ne 0) { Fail "cross-build $pair" }
        Write-Host "  ok $pair"
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
    }
}

Write-Host "`nOK CI PASSED  $version ($commit)" -ForegroundColor Green
exit 0
