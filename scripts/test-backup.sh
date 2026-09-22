#!/usr/bin/env bash
# Prove deploy/backup.sh end to end against a sandbox tree that has production's
# layout, without touching a real host:
#
#   bash scripts/test-backup.sh
#
# It is here because the scheduled sweep now refuses to prune rows unless a backup
# succeeded within 36 hours (see store.BackupPatience) - so this shell script is on
# the critical path of a data-deleting feature, and until now nothing had ever run
# it except production.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

BIN=${DH_TEST_BIN:-./dist/dealhunter}
[[ -x $BIN || -x "$BIN.exe" ]] || { echo "! 找不到 $BIN，先跑 scripts/ci-local.sh 的 build 步骤"; exit 1; }
[[ -x $BIN ]] || BIN=./dist/dealhunter.exe

ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
STATE="$ROOT/var/lib/deal-hunter"
ETC="$ROOT/etc/deal-hunter"
mkdir -p "$STATE" "$ETC"

fails=0
ok() { printf '  ok %s\n' "$*"; }
bad() { printf '  ✗ %s\n' "$*" >&2; fails=$((fails + 1)); }

# backup.sh sets POSIX modes (install -d -m 0750, umask 077, and the archive must keep
# the env file at 0600). On Windows those calls cannot succeed at all - that is a fact
# about the host, not about the script - so the drill says it skipped instead of
# reporting a red that nobody could act on.
probe=$(mktemp -d)
if ! install -d -m 0750 "$probe/x" 2>/dev/null; then
	rm -rf "$probe"
	printf '\033[1;33m! backup drill skipped: 本机无法设置 POSIX 权限（需要 Linux）\033[0m\n'
	exit 0
fi
rm -rf "$probe"

# --- the sandbox: a store, a config, and an env file whose mode must not widen ---
# Written with printf, not a scripting language: a gate step must not lean on a
# runtime the builders do not guarantee.
: > "$STATE/deals.jsonl"
for i in 0 1 2 3 4; do
	printf '{"fingerprint":"drill%05d","url":"https://example.test/%05d","title":"drill row %05d","source":"drill","category":"ai_free","score":%d,"is_free":true,"published_at":"2026-09-22T12:00:00Z","discovered_at":"2026-09-22T12:00:00Z","meta":{}}\n' \
		"$i" "$i" "$i" "$((70 - i))" >> "$STATE/deals.jsonl"
done
printf '%s' '{"sources":[],"server":{"enabled":false,"bind":"127.0.0.1:0"}}' > "$ETC/config.json"
# A placeholder, not a credential: the point is the file's *mode* inside the archive.
printf '%s\n' 'DH_FEISHU_WEBHOOK=placeholder-for-the-backup-drill' 'DH_DATA_DIR=/var/lib/deal-hunter' > "$ETC/deal-hunter.env"
chmod 600 "$ETC/deal-hunter.env"

# From here on, a failing command is a *result*, not a reason to stop: every leg
# reports through ok/bad and the exit code comes from the counter at the bottom.
# With `set -e` (plus pipefail) any `x=$(ls pattern-that-matches-nothing | wc -l)`
# aborts the whole drill - precisely in the case the counting leg exists to catch,
# so a product regression would report fewer reds, not more. The fixture above still
# runs under -e: a half-built sandbox should not produce twenty bogus failures.
set +e

echo "▸ backup.sh 在假根 $ROOT 下能跑完"
out=$(DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=2 bash deploy/backup.sh 2>&1) || { bad "第一次备份就失败：$out"; echo "$out"; exit 1; }
grep -q "归档可解开" <<<"$out" && ok "自校验通过（解得开、config.json 在位）" || bad "没有报可解开：$out"

# Three runs, one second apart: the archive name is a second-resolution stamp, so
# the sleep is what makes retention observable at all (not a timing assertion).
for n in 2 3; do
	sleep 1.2
	out=$(DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=2 bash deploy/backup.sh 2>&1) || { bad "第 $n 次备份失败：$out"; }
done
have=$(ls -1 "$ROOT/var/backups/deal-hunter"/*.tar.gz 2>/dev/null | wc -l | tr -d ' ')
[[ $have == "2" ]] && ok "KEEP=2 跑三次后只剩 2 份归档" || bad "KEEP=2 应剩 2 份，实际 $have 份"
sides=$(ls -1 "$ROOT/var/backups/deal-hunter"/*.sha256 2>/dev/null | wc -l | tr -d ' ')
[[ $sides == "2" ]] && ok "校验和随归档一起轮换（2 个 sidecar）" || bad "sidecar 数应为 2，实际 $sides"

# "Which one is newest" comes from the script's own completion line rather than from
# an ordering this file shares with the thing under test.
newest=$(grep '备份完成' <<<"$out" | grep -o '/[^ ]*\.tar\.gz' | head -1)
[[ -f $newest ]] && ok "报出来的归档确实在盘上（$(basename "$newest")）" || bad "完成行指的文件不存在：「$out」"
# Keep going from whatever is really there even if that line moved, so a failure
# above reports the missing file instead of crashing the rest of the drill.
[[ -f $newest ]] || newest=$(ls -1t "$ROOT/var/backups/deal-hunter"/deal-hunter-*.tar.gz 2>/dev/null | head -1)
# Retention reads names only to match the glob, but operators (and this file) read
# them to tell recency apart, so the shape is a contract.
base=$(basename "${newest:-none}")
[[ $base =~ ^deal-hunter-[0-9]{8}-[0-9]{6}\.tar\.gz$ ]] \
	&& ok "归档名是 deal-hunter-<UTC日期>-<时刻> 的形状" \
	|| bad "归档名形状变了：「$base」（日期里多出的分隔符会让按名字排序的轮换认错最新那份）"
oldest_kept=$(ls -1t "$ROOT/var/backups/deal-hunter"/deal-hunter-*.tar.gz | tail -1)
[[ $newest != "$oldest_kept" ]] && ok "留下的是两份不同的（即最新的两个），不是同一份被数两次"

echo "▸ 校验和与恢复"
bdir=$(dirname "$newest")
( cd "$bdir" && sha256sum --quiet --check "$(basename "$newest").sha256" ) \
	&& ok "最新归档的 sha256 校验通过" || bad "最新归档校验失败"
X=$(mktemp -d)
tar -xzf "$newest" -C "$X"
back=$(wc -l < "$X/var/lib/deal-hunter/deals.jsonl" | tr -d ' ')
live=$(wc -l < "$STATE/deals.jsonl" | tr -d ' ')
[[ $back == "$live" ]] && ok "解出来的行数与在线一致（$back/$live）" || bad "行数不一致 $back/$live"
[[ -f $X/etc/deal-hunter/config.json ]] && ok "config.json 在归档里" || bad "归档里没有 config.json"
mode=$(tar -tzvf "$newest" etc/deal-hunter/deal-hunter.env | awk '{print $1}')
[[ $mode == "-rw-------" ]] && ok "env 文件在归档里仍是 0600" || bad "env 文件模式变成了 $mode"
# The archive itself is the artifact that carries the webhook and its signing secret,
# so *its* mode is the one that matters: umask 077 is what keeps it off the group.
arch_mode=$(stat -c '%a' "$newest")
[[ $arch_mode == "600" || $arch_mode == "400" ]] \
	&& ok "归档本身是 $arch_mode（含密钥的包不会放宽给同组或其他人）" \
	|| bad "归档权限是 $arch_mode，应为 0600（umask 077 没起作用？）"

echo "▸ 备份记号必须能被 Go 侧读出来（压缩排程就认这个）"
[[ -f $STATE/backup.stamp ]] && ok "backup.stamp 已写下" || bad "没有 backup.stamp，排程会永远拒绝压缩"
d_out=$(DH_DATA_DIR="$STATE" $BIN -config "$ETC/config.json" doctor -net=false 2>&1) || true
grep -q "✓   backup" <<<"$d_out" && ok "doctor 读得懂这个记号（✓ backup）" || bad "doctor 没认可它：$(grep backup <<<"$d_out")"

echo "▸ 危险配置必须被拒绝，而不是清空目录"
before=$(ls -1 "$bdir" | wc -l | tr -d ' ')
if DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=0 bash deploy/backup.sh >/dev/null 2>&1; then
	bad "KEEP=0 没有被拒绝"
else
	after=$(ls -1 "$bdir" | wc -l | tr -d ' ')
	[[ $after == "$before" ]] && ok "KEEP=0 被拒绝且一个文件都没少（$after 个）" || bad "KEEP=0 之后目录从 $before 变成 $after"
fi
DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=abc bash deploy/backup.sh >/dev/null 2>&1 \
	&& bad "KEEP=abc 没有被拒绝" || ok "KEEP 非数字被拒绝"

echo "▸ 归档缺 config.json 时不能报成功"
rm "$ETC/config.json"
if DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=2 bash deploy/backup.sh >/dev/null 2>&1; then
	bad "没有 config.json 也报了成功"
else
	ok "缺 config.json 时脚本拒绝"
fi
printf '%s' '{"sources":[],"server":{"enabled":false,"bind":"127.0.0.1:0"}}' > "$ETC/config.json"

echo "▸ 轮换认的是「谁新」，不是「名字排在后面」"
# A decoy whose name sorts after every real archive but which is three days old.
# Name-sorted retention calls the fresh archive the oldest thing in the directory and
# deletes the one backup we can restore from, so recency has to be measured, not read.
decoy="$bdir/deal-hunter-99999999-999999.tar.gz"
: > "$decoy"
printf '%s' 'deadbeef  deal-hunter-99999999-999999.tar.gz' > "$decoy.sha256"
touch -d '3 days ago' "$decoy" "$decoy.sha256"
sleep 1.2
DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=1 bash deploy/backup.sh >/dev/null 2>&1 || bad "带诱饵的那次备份失败了"
[[ ! -e $decoy ]] && ok "三天前那份被轮换掉了，尽管它的名字排在最后" \
	|| bad "诱饵还在：轮换在按名字排新旧，最新那份会被当成最旧的删掉"
survivor=$(ls -1 "$bdir"/deal-hunter-*.tar.gz 2>/dev/null | head -1)
n_surv=$(ls -1 "$bdir"/deal-hunter-*.tar.gz 2>/dev/null | wc -l | tr -d ' ')
[[ $n_surv == "1" && -n $survivor ]] && ok "KEEP=1 只剩 1 份归档" || bad "KEEP=1 应剩 1 份，实际 $n_surv"
( cd "$bdir" && sha256sum --quiet --check "$(basename "$survivor").sha256" ) \
	&& ok "活下来的那份校验得过（是刚做的备份，不是诱饵）" \
	|| bad "活下来的那份校验不过——留下的可能是诱饵"

echo "▸ 另存一份到别处（DH_BACKUP_PUSH），以及它失败时该留下什么"
# Production has never set DH_BACKUP_PUSH, so this branch had never been executed
# anywhere - while it is exactly the leg that would protect the backups from the disk
# they currently share with the service.
offsite="$ROOT/offsite"
mkdir -p "$offsite"
sleep 1.2
rc=0
out=$(DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=1 DH_BACKUP_PUSH="$offsite" bash deploy/backup.sh 2>&1) || rc=$?
(( rc == 0 )) && ok "带 PUSH 的备份跑完了" || bad "带 PUSH 的备份失败（rc=$rc）：$out"
grep -q '已另存一份' <<<"$out" && ok "报出了另存这一步" || bad "没有另存的记录：$out"
[[ $(ls -1 "$offsite" 2>/dev/null | wc -l | tr -d ' ') == "2" ]] \
	&& ok "归档与校验和都到了另一处（2 个文件）" || bad "另存目录里有 $(ls -1 "$offsite" 2>/dev/null | wc -l | tr -d ' ') 个文件，应为 2"
pushed=$(ls -1 "$offsite"/deal-hunter-*.tar.gz 2>/dev/null | head -1)
[[ -n $pushed ]] && ( cd "$offsite" && sha256sum --quiet --check "$(basename "$pushed").sha256" ) \
	&& ok "另存那份的校验和自洽" || bad "另存那份校验不过"
# 只数文件数会放过"推了一个空壳过去"，所以拿字节比。
[[ -n $pushed && -f $bdir/$(basename "$pushed") ]] && cmp -s "$pushed" "$bdir/$(basename "$pushed")" \
	&& ok "另存与本地是同一份字节" || bad "两份内容不同（或缺了本地那份）"

# `out=$(...) || rc=$?` because `set -e` would otherwise end the drill here, and the
# status has to come from backup.sh rather than from the assignment succeeding.
before=$(ls -1 "$bdir"/deal-hunter-*.tar.gz | wc -l | tr -d ' ')
stamp_before=$(cat "$STATE/backup.stamp")
sleep 1.2
rc=0
out=$(DH_BACKUP_ROOT="$ROOT" DH_BACKUP_KEEP=1 DH_BACKUP_PUSH="$ROOT/no-such-place" bash deploy/backup.sh 2>&1) || rc=$?
(( rc != 0 )) && ok "推送到不存在的目标时整轮以非零退出（定时器会留下失败记录）" \
	|| bad "推送没成功却报了 0"
grep -q '推送到' <<<"$out" && ok "点名叫出是推送这一步失败" || bad "没有说明失败原因：$out"
# A round that dies at the push still leaves its own archive behind (rotation happens
# after the push), so what must hold is "nothing was lost", not "the count is frozen".
# The stronger claim is the one the sweep leans on: the archive backup.stamp names has
# to still be on disk.
n_after=$(ls -1 "$bdir"/deal-hunter-*.tar.gz | wc -l | tr -d ' ')
(( n_after >= before )) && ok "本机一份都没少（$before → $n_after，失败那轮只会多不会丢）" \
	|| bad "跨机失败把本地的也带坏了（$before → $n_after）"
named=$(awk '{print $2}' "$STATE/backup.stamp")
[[ -n $named && -f "$bdir/$named" ]] \
	&& ok "记号指的那份归档仍在盘上（$named）" || bad "backup.stamp 指着一个不存在的归档：「$named」"
# The stamp is the last thing a run writes, so a round that failed halfway must not
# leave "success" behind - the sweep reads it as the reason it is allowed to delete.
[[ $(cat "$STATE/backup.stamp") == "$stamp_before" ]] \
	&& ok "失败那一轮没有改写 backup.stamp（排程仍认上一次完整成功的时刻）" \
	|| bad "失败的一轮把记号也刷了：$(cat "$STATE/backup.stamp")"

echo

if (( fails > 0 )); then
	printf '\033[1;31m✗ backup drill: %d 条不通过\033[0m\n' "$fails"
	exit 1
fi
printf '\033[1;32m✓ backup drill passed\033[0m\n'
