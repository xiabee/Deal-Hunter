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

step "commit identity scan (no personal mailbox in recent history)"
bash scripts/check-identity.sh || fail "a commit records a personal mailbox"

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

# Every httpapi test drives the handler through httptest, so nothing but this step
# ever runs Serve(): the bind, the enabled gate, and the -hold shutdown path.
step "served panel: the real listener answers, then exits on its own"
SERVE_DIR="$(mktemp -d)"
printf '%s' '{"sources":[],"server":{"enabled":true,"bind":"127.0.0.1:0"}}' > "$SERVE_DIR/config.json"
DH_DATA_DIR="$SERVE_DIR" ./dist/dealhunter -config "$SERVE_DIR/config.json" once -serve -hold 20s \
	>"$SERVE_DIR/serve.log" 2>&1 &
serve_pid=$!
serve_done() {
	kill "$serve_pid" 2>/dev/null || true
	wait "$serve_pid" 2>/dev/null || true
	rm -rf "$SERVE_DIR"
}
serve_fail() {
	serve_done
	fail "$@"
}

serve_url=""
for _ in $(seq 1 200); do
	line="$(grep -m1 -oE 'url=http://127\.0\.0\.1:[0-9]+' "$SERVE_DIR/serve.log" 2>/dev/null || true)"
	if [[ -n "$line" ]]; then
		serve_url="${line#url=}"
		break
	fi
	kill -0 "$serve_pid" 2>/dev/null || break
	sleep 0.1
done
[[ -n "$serve_url" ]] || {
	cat "$SERVE_DIR/serve.log"
	serve_fail "the panel never reported a listening URL"
}
echo "  listening on $serve_url"

# A code that is not a number means the client never got a response at all;
# treating that as "not 200" would hide a dead listener behind a wrong status.
for path in /healthz / /api/v1/status /api/v1/sources; do
	code="$(curl -s -o /dev/null -w '%{http_code}' "$serve_url$path" 2>/dev/null || true)"
	case "$code" in
	200) ;;
	'' | *[!0-9]*) serve_fail "GET $path returned no HTTP status at all" ;;
	*) serve_fail "GET $path over the real listener = $code, want 200" ;;
	esac
done
echo "  healthz, panel, status and sources all answered 200"

panel="$(curl -fsS "$serve_url/" 2>/dev/null || true)"
theme_hits="$(grep -c 'data-theme' <<<"$panel" || true)"
[[ "${theme_hits:-0}" -ge 1 ]] || serve_fail "the served panel lost its theme switch"
status_body="$(curl -fsS "$serve_url/api/v1/status" 2>/dev/null || true)"
[[ "$status_body" == *'"live_in_briefing"'* ]] || serve_fail "status payload lost live_in_briefing"
echo "  panel HTML carries the theme switch ($theme_hits hits) and status carries the briefing count"

# The loopback-only rule must hold on the real socket, not just in Validate's unit test.
printf '%s' '{"sources":[],"server":{"enabled":true,"bind":"0.0.0.0:0"}}' > "$SERVE_DIR/public.json"
if pub_out="$(DH_DATA_DIR="$SERVE_DIR" ./dist/dealhunter -config "$SERVE_DIR/public.json" once -serve -hold 2s 2>&1)"; then
	printf '%s\n' "$pub_out"
	serve_fail "a public bind started without complaint"
fi
if [[ "$pub_out" == *"listening"* ]]; then
	serve_fail "a public bind reached the listener despite the guard"
fi
echo "  a public bind is refused before anything listens"

rc=0
wait "$serve_pid" || rc=$?
[[ "$rc" == "0" ]] || serve_fail "the panel exited $rc instead of shutting down cleanly at -hold"
if err_line="$(grep -m1 'level=ERROR' "$SERVE_DIR/serve.log")"; then
	serve_fail "the served round logged an error: $err_line"
fi
serve_done
echo "  exited 0 when -hold expired, with no ERROR in the log"

# The scheduled sweep refuses to prune unless a backup succeeded within 36 hours, so
# deploy/backup.sh sits on the critical path of a data-deleting feature. It had never
# been run by anything but production.
step "backup drill: sandbox restore, retention and refusals"
bash scripts/test-backup.sh || fail "backup drill"

if [[ "$QUICK" != "1" ]]; then
	step "cross-compile (deploy targets)"
	for pair in linux/amd64 linux/arm64 windows/amd64; do
		goos="${pair%/*}"
		goarch="${pair#*/}"
		(
			export GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0
			go build -trimpath -ldflags "$LDFLAGS" -o "dist/dealhunter-${goos}-${goarch}" ./cmd/dealhunter
		) || fail "cross-build $pair"
		echo "  ok $pair"
	done
fi

printf '\n\033[1;32m✓ CI PASSED\033[0m  %s (%s)\n' "$VERSION" "$COMMIT"
