#!/usr/bin/env bash
# Drill for deploy/rollback-prev.sh - the move that makes a failed upgrade end with
# the host running something that answered, instead of sitting on the binary that
# just failed the health check.
#
# Runs against a fake prefix (DH_DEPLOY_PREFIX) and a stub systemctl on PATH, so the
# destructive half is exercised for real without a service to disrupt. install.sh's
# own wiring back to this script is asserted from its text: that file needs root and
# systemd and cannot run in a gate.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BIN=./dist/dealhunter
[[ -x "$BIN" ]] || BIN=./dist/dealhunter.exe
[[ -x "$BIN" ]] || { echo "! 没有 dist/dealhunter，先跑构建步骤" >&2; exit 1; }
ROLL=./deploy/rollback-prev.sh

ok_n=0
bad_n=0
pass() { ok_n=$((ok_n + 1)); printf '  ok %s\n' "$1"; }
bad() { bad_n=$((bad_n + 1)); printf '  ! %s\n' "$1" >&2; }

DIR="$(mktemp -d)"
STUB="$DIR/bin"
mkdir -p "$STUB" "$DIR/prefix"
CALLS="$DIR/systemctl-calls"
cat >"$STUB/systemctl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$CALLS"
EOF
chmod +x "$STUB/systemctl"
export PATH="$STUB:$PATH"

fresh_prefix() { # live-binary prev-binary
	local p="$1"
	rm -rf "$p"; mkdir -p "$p"
	: >"$CALLS"
}

# ---- the happy path: prev becomes live, the service is restarted once ----------
P="$DIR/prefix/ok"; fresh_prefix "$P"
printf 'the-new-one-that-failed\n' > "$P/deal-hunter"
cp "$BIN" "$P/deal-hunter.bak-prev"
if out="$(DH_DEPLOY_PREFIX="$P" bash "$ROLL" 2>&1)"; then
	if grep -q 'the-new-one-that-failed' "$P/deal-hunter"; then
		bad "rollback said ok but left the rejected binary live"
	else
		pass "退回后 $P/deal-hunter 是上一版那份字节"
	fi
	calls="$(wc -l < "$CALLS" | tr -d ' ')"
	if [[ "$calls" == "1" ]] && grep -qx "restart deal-hunter" "$CALLS"; then
		pass "重启恰好一次，命令是 restart deal-hunter"
	else
		bad "systemctl calls wrong: [$calls] $(tr '\n' '|' < "$CALLS")"
	fi
	[[ -e "$P/deal-hunter.bak-prev" ]] && bad "退回来的那份又被留在盘上（每次都多一份副本，就是老流程攒出 27 个的原因）" \
		|| pass "不留第二份副本：上一版换上来后就占住 live 的位置"
else
	cat <<<"$out"
	bad "the happy path refused to roll back"
fi

# ---- no previous copy: say so, and touch nothing -------------------------------
P="$DIR/prefix/noprev"; fresh_prefix "$P"
printf 'only-version-live\n' > "$P/deal-hunter"
out="$(DH_DEPLOY_PREFIX="$P" bash "$ROLL" 2>&1)"; rc=$?
if [[ "$rc" != 0 ]] && grep -q "首次安装" <<<"$out"; then
	pass "没有上一版时拒绝退回，并说清这是首装场景"
else
	bad "expected a refusal naming 首次安装, rc=$rc out=$out"
fi
grep -q 'only-version-live' "$P/deal-hunter" || bad "拒绝退回却动了 live 二进制"
[[ ! -s "$CALLS" ]] || bad "拒绝退回时不该重启服务：$(tr '\n' '|' < "$CALLS")"

# ---- a prev that cannot run must not become live -------------------------------
# This is the whole reason the script asks the file "version" instead of trusting a
# name: rolling back onto a second broken binary turns a failed upgrade into an
# outage that looks intentional.
P="$DIR/prefix/brokenprev"; fresh_prefix "$P"
printf 'live-new\n' > "$P/deal-hunter"
printf '#!/usr/bin/env false\n' > "$P/deal-hunter.bak-prev"
chmod +x "$P/deal-hunter.bak-prev"
out="$(DH_DEPLOY_PREFIX="$P" bash "$ROLL" 2>&1)"; rc=$?
if [[ "$rc" != 0 ]] && grep -q "跑不起来" <<<"$out"; then
	pass "上一版自己跑不起来时不退回"
else
	bad "expected a refusal for an un-runnable prev, rc=$rc out=$out"
fi
grep -q 'live-new' "$P/deal-hunter" || bad "拒绝退回却动了 live 二进制"
[[ -s "$P/deal-hunter.bak-prev" ]] || bad "被拒的那份不该被搬走"

# ---- a prev that is not executable at all -------------------------------------
P="$DIR/prefix/nonexec"; fresh_prefix "$P"
printf 'live-new2\n' > "$P/deal-hunter"
cp "$BIN" "$P/deal-hunter.bak-prev"
chmod -x "$P/deal-hunter.bak-prev" 2>/dev/null || true
if [[ -x "$P/deal-hunter.bak-prev" ]]; then
	pass "跳过不可执行那条：本机改不动执行位（$(uname -s)）"
else
	out="$(DH_DEPLOY_PREFIX="$P" bash "$ROLL" 2>&1)"; rc=$?
	if [[ "$rc" != 0 ]] && grep -q "不可执行" <<<"$out"; then
		pass "上一版没有执行位时拒绝退回"
	else
		bad "expected a refusal for a non-executable prev, rc=$rc out=$out"
	fi
fi

# ---- no systemctl: the swap happens, and that is said out loud ----------------
# Silently skipping the restart is how you end up with a rolled-back binary that no
# process is running. The script must exit non-zero and name the command to run.
P="$DIR/prefix/nosystemctl"; fresh_prefix "$P"
printf 'live3\n' > "$P/deal-hunter"
cp "$BIN" "$P/deal-hunter.bak-prev"
out="$(PATH="/usr/bin:/bin" DH_DEPLOY_PREFIX="$P" bash "$ROLL" 2>&1)"; rc=$?
if [[ "$rc" != 0 ]] && grep -q "systemctl restart" <<<"$out" && ! grep -q 'live3' "$P/deal-hunter"; then
	pass "没有 systemctl 时照样换回上一版，但非零退出并给出该跑的命令"
else
	bad "expected a loud swap-without-restart, rc=$rc out=$out"
fi

# ---- install.sh must actually call it on the failed-health path ----------------
if grep -q 'rollback-prev.sh' deploy/install.sh; then
	block="$(grep -A 10 'wait-healthy.sh" "http' deploy/install.sh)"
	if grep -q 'rollback-prev.sh' <<<"$block"; then
		pass "install.sh 的健康检查失败那一路调用了退回（而不是只 die）"
	else
		bad "install.sh ships the script but does not call it where the check fails"
	fi
else
	bad "install.sh never mentions rollback-prev.sh"
fi
if grep -q 'install -m 0755 "$REPO_ROOT/deploy/rollback-prev.sh"' deploy/install.sh; then
	pass '退回脚本随部署装到 /opt/deal-hunter（运维可以手跑）'
else
	bad "install.sh does not ship rollback-prev.sh to the host"
fi

echo
if [[ "$bad_n" == "0" ]]; then
	printf '\xe2\x9c\x93 rollback drill passed (%d checks)\n' "$ok_n"
	exit 0
fi
printf '\xe2\x9c\x97 rollback drill failed (%d problems, %d passed)\n' "$bad_n" "$ok_n" >&2
exit 1
