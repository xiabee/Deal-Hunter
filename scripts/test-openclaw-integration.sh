#!/usr/bin/env bash
# Prove deploy/openclaw/install-openclaw-integration.sh keeps exactly one previous
# copy of what it overwrites - and refuses to overwrite when that copy could not be
# written:
#
#   bash scripts/test-openclaw-integration.sh
#
# It is the one installer in deploy/ that rewrites files it does not own (an
# operator's OpenClaw workspace and skill file), and it takes its target locations
# from the environment, so the whole thing runs against a throwaway directory with
# no root and no service.
#
# No `set -e`: half of these legs exist to make the installer fail on purpose.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ $(uname -s) != Linux ]]; then
	printf '\033[1;33m! openclaw drill skipped: 需要 Linux（"写不进去"那种情形是靠 POSIX 权限位造的）\033[0m\n'
	exit 0
fi

ROOT=$(mktemp -d)
# The failure legs leave $BACKUP_ROOT read-only on purpose; put the undo in the trap
# so the cleanup never depends on which leg managed to reach its own chmod.
trap 'chmod -R u+rwx "$ROOT" 2>/dev/null; rm -rf "$ROOT"' EXIT
export OPENCLAW_HOME="$ROOT/oc"
export OPENCLAW_SKILLS_DIR="$ROOT/skills"
export BACKUP_ROOT="$ROOT/bak"
WS="$OPENCLAW_HOME/workspace/deal-hunter"
TGZ="$BACKUP_ROOT/workspace-deal-hunter.tgz"

fails=0
ok() { printf '  ok %s\n' "$*"; }
bad() { printf '  ✗ %s\n' "$*" >&2; fails=$((fails + 1)); }
inst() { bash deploy/openclaw/install-openclaw-integration.sh >"$ROOT/last.log" 2>&1; }
# A control that never reaches the code under test reads like a pass. Before any leg
# that expects the installer to fail, show the write really does fail there - an
# unwritable *directory* is not enough, because opening an existing writable file
# needs no permission on the directory holding it.
refuses() { "$@" >"$ROOT/probe.log" 2>&1; }

echo "▸ 第一次装：没有东西要备份，就不该产生副本"
inst
(( $? == 0 )) && ok "空目录安装成功" || bad "第一次就失败：$(tail -3 "$ROOT/last.log")"
[[ $(ls -1 "$WS" 2>/dev/null | wc -l | tr -d ' ') == "4" ]] \
	&& ok "workspace 里四个文件都在（三个查询脚本 + 技能说明）" || bad "装出来的文件数不是四个：$(ls -1 "$WS" 2>/dev/null | tr '\n' ' ')"
[[ $(ls -1 "$BACKUP_ROOT" 2>/dev/null | wc -l | tr -d ' ') == "0" ]] \
	&& ok "无东西可备份时不产生副本" || bad "还没覆盖就先留了副本"

echo "▸ 第二次装：被覆盖掉的本地改动必须在那唯一一份快照里"
echo LOCAL-EDIT-MARKER > "$WS/note.txt"
mkdir -p "$OPENCLAW_SKILLS_DIR"
echo OLD-SKILL-TEXT > "$OPENCLAW_SKILLS_DIR/deal-hunter.skill.md"
inst
(( $? == 0 )) && ok "覆盖安装成功" || bad "第二次失败：$(tail -3 "$ROOT/last.log")"
[[ -f $TGZ ]] && ok "上一份 workspace 在 workspace-deal-hunter.tgz" || bad "没有 workspace 快照"
tar -tzf "$TGZ" 2>/dev/null | grep -q 'deal-hunter/note.txt' \
	&& ok "快照里带着被覆盖前的本地改动（note.txt 在包内）" || bad "快照没接住任何东西"
grep -q OLD-SKILL-TEXT "$BACKUP_ROOT/deal-hunter.skill.md.bak" 2>/dev/null \
	&& ok "技能说明的上一份是被覆盖掉的那一版" || bad "技能说明的副本内容不对"

echo "▸ 第三次装：数量不再涨，且那一份必须刚刚刷新过"
# "Only one file" alone would also be satisfied by a backup that stopped happening.
echo RUN3-MARKER > "$WS/run3.txt"
inst
(( $? == 0 )) && ok "重复安装无碍" || bad "第三次失败：$(tail -3 "$ROOT/last.log")"
[[ $(ls -1 "$BACKUP_ROOT"/*.tgz 2>/dev/null | wc -l | tr -d ' ') == "1" ]] \
	&& ok "跑了三次盘上仍是一份 workspace 快照" || bad "快照按次数累积回来了"
[[ $(ls -1 "$BACKUP_ROOT"/*.bak 2>/dev/null | wc -l | tr -d ' ') == "1" ]] \
	&& ok "跑了三次盘上仍是一份技能说明" || bad "技能副本按次数累积回来了"
tar -tzf "$TGZ" 2>/dev/null | grep -q 'deal-hunter/run3.txt' \
	&& ok "那一份是第三次之前的状态，不是上上次的" || bad "快照没随安装刷新：一份是好事，停在旧内容上就不是备份"

echo "▸ 备份写不进去时必须拒绝覆盖（workspace 这一路）"
echo MODIFIED-BY-HAND > "$WS/dealhunter-status.sh"
chmod 500 "$BACKUP_ROOT"
chmod 444 "$BACKUP_ROOT"/*.tgz "$BACKUP_ROOT"/*.bak 2>/dev/null
if refuses tar -czf "$TGZ" -C "$OPENCLAW_HOME/workspace" deal-hunter; then
	bad "前提不成立：那个位置仍写得动，这一腿什么也没测"
else
	ok "前提成立：往那个位置写 tar 确实会失败"
	inst
	(( $? != 0 )) && ok "备份失败时脚本非零退出" || bad "备份没成功却报了 0"
	grep -q MODIFIED-BY-HAND "$WS/dealhunter-status.sh" 2>/dev/null \
		&& ok "安装器要写的那个文件没被动过" || bad "备份失败了还是把 workspace 覆盖了"
	grep -q '先停在这里' "$ROOT/last.log" && ok "说清了停在哪一步、为什么" || bad "没有给出原因"
fi
chmod 700 "$BACKUP_ROOT"
chmod 644 "$BACKUP_ROOT"/*.tgz "$BACKUP_ROOT"/*.bak 2>/dev/null

echo "▸ 只有技能说明这一路也要拒绝覆盖"
rm -rf "$OPENCLAW_HOME"
echo SKILL-KEEP-MARKER > "$OPENCLAW_SKILLS_DIR/deal-hunter.skill.md"
chmod 500 "$BACKUP_ROOT"
chmod 444 "$BACKUP_ROOT"/*.bak 2>/dev/null
if refuses cp -f /etc/hostname "$BACKUP_ROOT/deal-hunter.skill.md.bak"; then
	bad "前提不成立：技能说明的副本仍写得动，这一腿什么也没测"
else
	ok "前提成立：cp -f 往那个位置写确实会失败"
	inst
	(( $? != 0 )) && ok "技能说明备份不了时非零退出" || bad "没退出"
	grep -q SKILL-KEEP-MARKER "$OPENCLAW_SKILLS_DIR/deal-hunter.skill.md" \
		&& ok "已存在的技能说明没被换掉" || bad "备份失败了还是把技能说明覆盖了"
fi

echo
if (( fails > 0 )); then
	printf '\033[1;31m✗ openclaw installer drill: %d 条不通过\033[0m\n' "$fails"
	exit 1
fi
printf '\033[1;32m✓ openclaw installer drill passed\033[0m\n'
