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
# Overridable for the same reason VERSION is: scripts/ci-office.sh ships a tarball
# with no .git in it, and an artifact whose stamp says "none" cannot be traced back
# to the commit it was built from - which is the one thing the deploy check asks.
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
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

# `serve` is its own documented command ("只开面板不采集"), and until now no step had
# ever run it: the check above goes in through `once -serve`. Two promises live in that
# one command - it shows the store this box already has, and it does not collect - and
# neither had ever been observed. The first is checked against a seeded row rather than
# an empty file; the second against a log line, with a settle window because "no round
# started" is a fact about this command, not a race to win.
step "serve: the panel command answers from the store and never collects"
SRV_DIR="$(mktemp -d)"
mkdir -p "$SRV_DIR/data"
printf '%s\n' '{"fingerprint":"gateserve1","url":"https://gate.test/serve","title":"gate fixture: a live free tier","summary":"open to all users","source":"gate","category":"ai_free","score":71,"is_free":true,"published_at":"2026-01-01T00:00:00Z","discovered_at":"2026-01-01T00:00:00Z","meta":{}}' > "$SRV_DIR/data/deals.jsonl"
printf '%s' '{"sources":[{"name":"gate-canary","kind":"rss","url":"http://127.0.0.1:1/x.rss","trust":1}],"server":{"enabled":true,"bind":"127.0.0.1:0"},"timezone":"UTC"}' > "$SRV_DIR/config.json"
DH_DATA_DIR="$SRV_DIR/data" ./dist/dealhunter -config "$SRV_DIR/config.json" serve \
	>"$SRV_DIR/serve.log" 2>&1 &
srv_pid=$!
srv_done() {
	kill "$srv_pid" 2>/dev/null || true
	wait "$srv_pid" 2>/dev/null || true
	rm -rf "$SRV_DIR"
}
srv_fail() {
	[[ -f "$SRV_DIR/serve.log" ]] && cat "$SRV_DIR/serve.log"
	srv_done
	fail "$@"
}

srv_url=""
for _ in $(seq 1 200); do
	line="$(grep -m1 -oE 'url=http://127\.0\.0\.1:[0-9]+' "$SRV_DIR/serve.log" 2>/dev/null || true)"
	if [[ -n "$line" ]]; then
		srv_url="${line#url=}"
		break
	fi
	kill -0 "$srv_pid" 2>/dev/null || break
	sleep 0.1
done
[[ -n "$srv_url" ]] || srv_fail "serve never reported a listening URL"
echo "  listening on $srv_url"

for path in /healthz /api/v1/deals; do
	code="$(curl -s -o /dev/null -w '%{http_code}' "$srv_url$path" 2>/dev/null || true)"
	case "$code" in
	200) ;;
	'' | *[!0-9]*) srv_fail "GET $path returned no HTTP status at all" ;;
	*) srv_fail "GET $path over the real listener = $code, want 200" ;;
	esac
done
deals_body="$(curl -fsS "$srv_url/api/v1/deals" 2>/dev/null || true)"
[[ "$deals_body" == *gateserve1* ]] || srv_fail "serve did not show the row already in the store it was pointed at"
echo "  the seeded row is served back out of the store"

sleep 1
if grep -qE 'round complete|source failed' "$SRV_DIR/serve.log"; then
	srv_fail "serve collected - the panel command is supposed to be read-only"
fi
echo "  no collection round was started"

# A flag `serve` does not have must be refused rather than swallowed. The command used
# to ignore its arguments entirely, so `serve -hold 5s` served forever while looking like
# it had obeyed. `timeout` bounds the probe so a regression that goes back to ignoring
# arguments reports "exited 124" instead of hanging the gate.
rc=0
timeout 10 env DH_DATA_DIR="$SRV_DIR/data" ./dist/dealhunter -config "$SRV_DIR/config.json" 	serve -hold 5s >"$SRV_DIR/reject.log" 2>&1 || rc=$?
[[ "$rc" == "2" ]] || {
	cat "$SRV_DIR/reject.log"
	fail "serve accepted a flag it does not have (-hold): exited $rc, want 2"
}
if grep -q 'listening' "$SRV_DIR/reject.log"; then
	cat "$SRV_DIR/reject.log"
	fail "serve listened even though it was handed a flag it does not have"
fi
echo "  a flag it does not have is refused with exit 2, before anything listens"

if err_line="$(grep -m1 'level=ERROR' "$SRV_DIR/serve.log")"; then
	srv_fail "serve logged an error while serving: $err_line"
fi

# Stopping on a signal is the other half of the promise, but it cannot be asserted
# where signals are not POSIX ones: Git Bash reports 143 for any kill regardless of
# what the program did with it. So that leg runs on the Linux gate (and on the office
# builder), and says plainly that it did not run here.
if [[ "$(uname -s)" == "Linux" ]]; then
	kill -TERM "$srv_pid" 2>/dev/null || srv_fail "could not signal the panel command"
	rc=0
	wait "$srv_pid" || rc=$?
	srv_done
	[[ "$rc" == "0" ]] || fail "serve exited $rc on SIGTERM; a handled stop must exit 0"
	echo "  stopped on a signal with exit 0 and no ERROR in the log"
else
	srv_done
	echo "  ! SIGTERM leg skipped: $(uname -s) 不传递 POSIX 信号（office 那份会跑）"
fi

# The scheduled sweep refuses to prune unless a backup succeeded within 36 hours, so
# deploy/backup.sh sits on the critical path of a data-deleting feature. It had never
# been run by anything but production.
step "backup drill: sandbox restore, retention and refusals"
bash scripts/test-backup.sh || fail "backup drill"

# The OpenClaw installer rewrites files it does not own. Same reason for a drill: it
# had only ever been run by hand, and the guard it needs (keep one previous copy, and
# refuse to overwrite when that copy could not be written) is the kind that silently
# rots back into "copy aside with a fresh timestamp" if nothing checks it.
step "openclaw installer drill: one previous copy, and refusal when it cannot be written"
bash scripts/test-openclaw-integration.sh || fail "openclaw installer drill"

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
