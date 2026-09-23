#!/usr/bin/env bash
# Drill for deploy/wait-healthy.sh - the check install.sh now fails an upgrade on.
#
# Why this exists as a permanent drill: the readiness probe sits on the deploy path
# of a service that deletes nothing but that, if it never came up, silently stops
# delivering anything. install.sh itself needs root and systemd and cannot run in a
# gate, so the two halves are checked separately: the probe against a real listener
# and a dead port, and install.sh's wiring back to the probe.
#
# Runs offline. Needs the built binary (dist/dealhunter) like the rest of the gate.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BIN=./dist/dealhunter
[[ -x "$BIN" ]] || BIN=./dist/dealhunter.exe
WAIT=./deploy/wait-healthy.sh
ok=0
fail_n=0

line() { printf '  %s %s\n' "$1" "$2"; }
pass() {
	ok=$((ok + 1))
	line ok "$1"
}
bad() {
	fail_n=$((fail_n + 1))
	line "!" "$1" >&2
}

DIR="$(mktemp -d)"
trap 'rm -rf "$DIR"' EXIT

# ---- the probe against a listener that is really there -----------------------
printf '%s' '{"sources":[],"server":{"enabled":true,"bind":"127.0.0.1:0"},"timezone":"UTC"}' >"$DIR/config.json"
DH_DATA_DIR="$DIR" "$BIN" -config "$DIR/config.json" serve >"$DIR/serve.log" 2>&1 &
pid=$!
url=""
for _ in $(seq 1 200); do
	line_hit="$(grep -m1 -oE 'url=http://127\.0\.0\.1:[0-9]+' "$DIR/serve.log" 2>/dev/null || true)"
	if [[ -n "$line_hit" ]]; then
		url="${line_hit#url=}"
		break
	fi
	kill -0 "$pid" 2>/dev/null || break
	sleep 0.1
done
if [[ -z "$url" ]]; then
	bad "the drill could not start a real listener, so the probe was never exercised"
	cat "$DIR/serve.log"
else
	if out="$("$WAIT" "$url" 20 2>&1)"; then
		pass "对真监听器返回 0，并说得出 200：$out"
	else
		bad "the live panel was not accepted by the probe: $out"
	fi
fi
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true

# ---- the probe against a port nobody holds -----------------------------------
# 127.0.0.1:1 refuses instantly; a trailing slash in the URL must not change that.
if out="$("$WAIT" "http://127.0.0.1:1/" 2 2>&1)"; then
	rc=0
else
	rc=$?
fi
[[ "$rc" == "1" ]] || bad "a dead port should exit 1, got $rc ($out)"
case "$out" in
*没有给出任何*响应*) pass "对死端口说「没有任何 HTTP 响应」，而不是把 000 当成一个状态码" ;;
*) bad "a refused connection should be named as no response at all, got: $out" ;;
esac

# ---- wrong arguments must be refused, not waited on --------------------------
while IFS='|' read -r label args expect; do
	out="$($WAIT $args 2>&1)"
	rc=$?
	if [[ "$rc" == "$expect" ]]; then
		pass "$label 退出 $expect"
	else
		bad "$label: exited $rc, want $expect ($out)"
	fi
done <<'TABLE'
缺地址||2
等待秒数不是整数|http://127.0.0.1:1 abc|2
非 http 的地址|https://127.0.0.1:1 1|2
TABLE

# ---- install.sh must treat a failed probe as a failed install ----------------
# The old shape warned and continued, so `installer rc=0` meant "systemd said active
# once". This is the wiring half of that claim; the probe itself is covered above.
# A call is matched by what only a *call* looks like (the URL argument), so the line
# that installs the script is not counted. Two calls is the honest shape since M48:
# the acceptance probe, then the same probe again to confirm the rollback restored
# service. What must not exist is a warn-only path that neither passes nor fails.
calls="$(grep -c 'wait-healthy\.sh" "http' deploy/install.sh)"
if [[ "$calls" == "2" ]]; then
	pass "健康检查恰好两处调用：验收一次、退回后复确认一次"
else
	bad "install.sh calls the probe $calls times, want 2 (acceptance + post-rollback confirmation)"
fi
if grep -qF 'if ! "$PREFIX/wait-healthy.sh"' deploy/install.sh; then
	pass "验收那一处是反向守卫（if ! 健康检查），不健康才走退回"
else
	bad "the acceptance call is not negated - a dead panel would read as success"
fi
block="$(grep -A 12 'wait-healthy\.sh" "http' deploy/install.sh | head -26)"
if grep -q 'die ' <<<"$block"; then
	pass "检查失败那一路走 die，安装判为失败"
else
	bad "the failed-probe branch must die, not warn: $block"
fi
first="$(grep -n 'wait-healthy\.sh" "http' deploy/install.sh | head -1 | cut -d: -f1)"
if (( first > 100 )); then
	pass "健康检查排在单元安装与重启之后（第 $first 行）"
else
	bad "the probe runs at line $first, before the service was even installed"
fi
if grep -q 'install -m 0755 "$REPO_ROOT/deploy/wait-healthy.sh"' deploy/install.sh; then
	pass '探针脚本随部署装到 /opt/deal-hunter（运维可以自己对着重跑）'
else
	bad "install.sh does not ship wait-healthy.sh to the host"
fi

echo
if [[ "$fail_n" == "0" ]]; then
	printf '\xe2\x9c\x93 install readiness drill passed (%d checks)\n' "$ok"
	exit 0
fi
printf '\xe2\x9c\x97 install readiness drill failed (%d problems, %d passed)\n' "$fail_n" "$ok" >&2
exit 1
