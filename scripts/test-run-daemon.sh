#!/usr/bin/env bash
# Drill for the `run` command - what the systemd unit actually executes, and the
# last subcommand no gate step had ever started (the served-panel step goes in
# through `once -serve`, so it never touches the scheduler loop or -no-first).
#
# Three claims:
#   1. `run` comes up: it says it is starting, opens the panel, and finishes a
#      round on its own - which is the "启动即跑一轮" behaviour the deploy relies on.
#   2. `-no-first` really skips that first round. Nothing else in the project reads
#      that flag, so a regression (it gets ignored) would only show up as the service
#      collecting twice at boot on a box where nobody looks.
#   3. a signal stops it cleanly with exit 0 (Linux only - see the note at the leg).
#
# Offline: both processes talk to a source URL nothing serves, and every channel
# that could reach out is switched off. The console backend stays on because the
# pipeline refuses to construct with zero channels - and that fact is asserted where
# it belongs (cmd/dealhunter's test), not here.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BIN=./dist/dealhunter
[[ -x "$BIN" ]] || BIN=./dist/dealhunter.exe
[[ -x "$BIN" ]] || { echo "! 没有 dist/dealhunter，先跑构建步骤" >&2; exit 1; }

ok_n=0
bad_n=0
pass() { ok_n=$((ok_n + 1)); printf '  ok %s\n' "$1"; }
bad() {
	bad_n=$((bad_n + 1))
	printf '  ! %s\n' "$1" >&2
}

DIR="$(mktemp -d)"
trap 'rm -rf "$DIR"' EXIT

cfg() {
	printf '%s' '{"timezone":"UTC","server":{"enabled":true,"bind":"127.0.0.1:0"},' \
		'"notify":{"console":true,"daily":{"enabled":false},"urgent":{"enabled":false},"event":{"enabled":false}},' \
		'"sources":[{"name":"gate-run-canary","kind":"rss","url":"http://127.0.0.1:1/x.rss","trust":1}],"interval":"1m"}'
}

# wait_for <file> <ERE> [tries] - bounded, returns 0 with the line, 1 on timeout.
wait_for() {
	local file="$1" pattern="$2" tries="${3:-120}" waited=0
	while ((waited < tries)); do
		local hit
		hit="$(grep -m1 -E "$pattern" "$file" 2>/dev/null || true)"
		if [[ -n "$hit" ]]; then
			printf '%s' "$hit"
			return 0
		fi
		sleep 0.25
		waited=$((waited + 1))
	done
	return 1
}

start_run() { # dir label extra args...
	local dir="$1" label="$2"
	shift 2
	mkdir -p "$dir/data"
	cfg >"$dir/config.json"
	DH_DATA_DIR="$dir/data" "$BIN" -config "$dir/config.json" run "$@" >"$dir/run.log" 2>&1 &
	RUN_PID=$!
	RUN_LOG="$dir/run.log"
	RUN_LABEL="$label"
}

stop_run() {
	[[ -n "${RUN_PID:-}" ]] || return 0
	kill "$RUN_PID" 2>/dev/null || true
	wait "$RUN_PID" 2>/dev/null || true
	RUN_PID=""
}
trap 'stop_run; rm -rf "$DIR"' EXIT

# ---- 1 and 3: the default start ------------------------------------------------
start_run "$DIR/a" "run"
hit="$(wait_for "$RUN_LOG" 'url=http://127[.]0[.]0[.]1:[0-9]+' || true)"
# wait_for hands back the whole log line; the URL is one token inside it.
url="$(printf '%s' "$hit" | grep -m1 -oE 'url=http://127[.]0[.]0[.]1:[0-9]+' || true)"
url="${url#url=}"
if [[ -z "$url" ]]; then
	cat "$RUN_LOG"
	bad "run never opened the panel"
else
	url="${url#url=}"
	code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$url/healthz" 2>/dev/null || true)"
	if [[ "$code" == "200" ]]; then
		pass "run 起了面板并应答 200（$url）"
	else
		bad "the panel run opened did not answer /healthz: '$code'"
	fi
fi

line="$(wait_for "$RUN_LOG" 'msg="round complete"' || true)"
if [[ -n "$line" ]]; then
	pass "启动即跑第一轮：$(printf '%s' "$line" | grep -oE 'trigger=[a-z]+' | head -1)"
else
	cat "$RUN_LOG"
	bad "run did not execute a first round (the deploy depends on this)"
fi
stop_run

if [[ "$(uname -s)" == "Linux" ]]; then
	start_run "$DIR/b" "run"
	if [[ -n "$(wait_for "$RUN_LOG" 'msg="round complete"' || true)" ]]; then
		kill -TERM "$RUN_PID" 2>/dev/null || true
		rc=0
		wait "$RUN_PID" || rc=$?
		RUN_PID=""
		if [[ "$rc" == "0" ]]; then
			pass "SIGTERM 之后以 0 退出（systemd stop 不该等到 TimeoutStopSec）"
		else
			bad "run exited $rc on SIGTERM, want a handled stop with 0"
		fi
	else
		bad "the second instance never completed a round, so the signal leg had nothing to stop"
	fi
	stop_run
else
	pass "跳过信号那条判决（$(uname -s) 不传递 POSIX 信号；Linux 侧由本机与 office 构建机执行）"
fi

# ---- 2: -no-first --------------------------------------------------------------
# Given the round its own log to write into, then insist that log stays empty of it.
start_run "$DIR/c" "run -no-first" -no-first
hit="$(wait_for "$RUN_LOG" 'url=http://127[.]0[.]0[.]1:[0-9]+' || true)"
url2="$(printf '%s' "$hit" | grep -m1 -oE 'url=http://127[.]0[.]0[.]1:[0-9]+' || true)"
if [[ -n "$url2" ]]; then
	# The window is 10s, not a stopwatch guess: the boot round on this same fixture
	# lands 4.5s after startup (measured 2026-09-23 - the canary fetch has to time
	# out first), and leg 1 above proves a round really does show up when it is
	# supposed to. Watching for 10s and requiring nothing is the assertion.
	if [[ -n "$(wait_for "$RUN_LOG" 'msg="round complete"' 40 || true)" ]]; then
		bad "-no-first still collected at boot - the flag is being ignored"
	else
		code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "${url2#url=}/healthz" 2>/dev/null || true)"
		if [[ "$code" == "200" ]]; then
			pass "-no-first 不跑第一轮，而这 10s 里面板一直在应答（所以"没有轮"不是进程死了）"
		else
			bad "-no-first opened the panel then stopped answering it: '$code' - the absence of a round proves nothing"
		fi
	fi
else
	cat "$RUN_LOG"
	bad "-no-first did not even open the panel"
fi
if grep -q "level=ERROR" "$RUN_LOG"; then
	bad "the -no-first run logged an ERROR: $(grep -m1 level=ERROR "$RUN_LOG")"
fi
stop_run

echo
if [[ "$bad_n" == "0" ]]; then
	printf '\xe2\x9c\x93 run drill passed (%d checks)\n' "$ok_n"
	exit 0
fi
printf '\xe2\x9c\x97 run drill failed (%d problems, %d passed)\n' "$bad_n" "$ok_n" >&2
exit 1
