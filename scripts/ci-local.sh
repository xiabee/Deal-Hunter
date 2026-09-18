#!/usr/bin/env bash
# Deal-Hunter local CI gate — the authoritative acceptance run.
#
# GitHub Actions is intentionally NOT used: the account's hosted quota is
# exhausted, so this script (and its Windows twin ci-local.ps1) is the gate.
# It runs on the laptop, on the office Linux box or inside CI containers, and
# needs no network: every collector test works from fixtures.
#
# Usage: scripts/ci-local.sh [--quick]   (--quick skips race + cross-build)
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

QUICK=0
[[ "${1:-}" == "--quick" ]] && QUICK=1
export GOFLAGS="-mod=mod"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
BUILT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

step() { printf '\n\033[1;36m▸ %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31m✗ CI FAILED: %s\033[0m\n' "$*" >&2; exit 1; }

step "go version"
go version || fail "go toolchain missing"

step "gofmt"
unformatted="$(gofmt -l cmd internal 2>/dev/null || true)"
[[ -n "$unformatted" ]] && { printf '%s\n' "$unformatted"; fail "files need gofmt"; }
echo "all files formatted"

step "go vet"
go vet ./... || fail "go vet"

step "build"
mkdir -p dist
LDFLAGS="-s -w -X github.com/xiabee/deal-hunter/internal/version.Version=${VERSION} -X github.com/xiabee/deal-hunter/internal/version.Commit=${COMMIT} -X github.com/xiabee/deal-hunter/internal/version.BuildDate=${BUILT}"
go build -trimpath -ldflags "$LDFLAGS" -o dist/dealhunter ./cmd/dealhunter || fail "build"

step "unit + integration tests (offline fixtures)"
if [[ "$QUICK" == "1" ]]; then
	go test ./... || fail "tests"
else
	go test -race -count=1 ./... || fail "race tests"
fi

step "secret scan (open-source release gate)"
go run ./cmd/dealhunter secretscan -C . || fail "secrets or private topology in the tree"

step "offline smoke: no network required"
# Bash pattern matching, not `cmd | grep -q`: grep exits on first match, which
# SIGPIPEs the binary and makes a healthy run fail under `set -o pipefail`.
SMOKE_DIR="$(mktemp -d)"

if ! v_out="$(DH_DATA_DIR="$SMOKE_DIR" ./dist/dealhunter version)"; then
	fail "version exited non-zero"
fi
[[ "$v_out" == *deal-hunter* ]] || fail "version smoke: $v_out"
echo "  $v_out"

if ! s_out="$(DH_DATA_DIR="$SMOKE_DIR" ./dist/dealhunter sources)"; then
	fail "sources exited non-zero"
fi
for want in openrouter-free-models search-ai-free-cn copilot-plans-snapshot aliyun-benefit; do
	[[ "$s_out" == *"$want"* ]] || fail "sources smoke: $want missing"
done
echo "  15 configured sources visible, autonomous collectors present"

if ! d_out="$(DH_DATA_DIR="$SMOKE_DIR" ./dist/dealhunter doctor -net=false)"; then
	fail "doctor reported failures:"$'\n'"$d_out"
fi
[[ "$d_out" == *体检通过* ]] || fail "doctor smoke: $d_out"
echo "  doctor passed with no network probes"
rm -rf "$SMOKE_DIR"

if [[ "$QUICK" != "1" ]]; then
	step "cross-compile (deploy targets)"
	for pair in linux/amd64 linux/arm64 windows/amd64; do
		goos="${pair%/*}"
		goarch="${pair#*/}"
		(
			export GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0
			go build -trimpath -ldflags "-s -w" -o "dist/dealhunter-${goos}-${goarch}" ./cmd/dealhunter
		) || fail "cross-build $pair"
		echo "  ok $pair"
	done
fi

printf '\n\033[1;32m✓ CI PASSED\033[0m  %s (%s)\n' "$VERSION" "$COMMIT"
