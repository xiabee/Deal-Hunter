#!/usr/bin/env bash
# 安装 Deal-Hunter 的 OpenClaw 联动脚本与技能说明。
#
# 强约束（与本机既有约定一致）：
#   · 不读取、不写入、不导出任何 OpenClaw 凭据；
#   · 不调用 openclaw agent / 不消耗模型 token；
#   · 不修改 OpenClaw 核心配置；
#   · 只放「本地只读 HTTP 查询脚本 + 技能说明」；
#   · 覆盖前先备份。
#
# 用法：以 OpenClaw 的运行用户执行
#   ./deploy/openclaw/install-openclaw-integration.sh
set -euo pipefail

OPENCLAW_HOME="${OPENCLAW_HOME:-$HOME/.openclaw}"
WS="$OPENCLAW_HOME/workspace/deal-hunter"
SKILLS_DIR="${OPENCLAW_SKILLS_DIR:-$OPENCLAW_HOME/skills}"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_ROOT="${BACKUP_ROOT:-$HOME/backups/deal-hunter}"

echo "[INFO] OpenClaw home: $OPENCLAW_HOME (user: $(id -un))"
if [[ ! -d "$OPENCLAW_HOME" ]]; then
	echo "[WARN] 未发现 OpenClaw 目录；仍会创建 workspace，装好 OpenClaw 后即可使用。"
fi

# One previous snapshot under a fixed name. These used to carry a fresh timestamp
# on every run and nothing reclaimed them, so the pile grew by a whole workspace
# each time this was re-installed. A backup exists to make the overwrite below
# survivable; if it cannot be written, the overwrite must not happen.
if [[ -d "$WS" ]]; then
	mkdir -p "$BACKUP_ROOT"
	if tar -czf "$BACKUP_ROOT/workspace-deal-hunter.tgz" -C "$(dirname "$WS")" "$(basename "$WS")"; then
		echo "[INFO] 旧 workspace 已备份到 $BACKUP_ROOT/workspace-deal-hunter.tgz（只留上一份）"
	else
		echo "[WARN] 旧 workspace 备份失败，先停在这里：不覆盖 $WS" >&2
		exit 1
	fi
fi

install -d -m 0755 "$WS"
for f in dealhunter-digest.sh dealhunter-deals.sh dealhunter-status.sh; do
	install -m 0755 "$SRC_DIR/$f" "$WS/$f"
done
install -m 0644 "$SRC_DIR/deal-hunter.skill.md" "$WS/deal-hunter.skill.md"
echo "[OK] 已安装到 $WS"

# 如果存在标准 skills 目录，按其规范放一份技能说明（只新增，不改核心配置）
if [[ -d "$SKILLS_DIR" ]]; then
	target="$SKILLS_DIR/deal-hunter.skill.md"
	if [[ -e "$target" ]]; then
		mkdir -p "$BACKUP_ROOT"
		if cp -f "$target" "$BACKUP_ROOT/deal-hunter.skill.md.bak"; then
			echo "[INFO] 技能说明的上一份在 $BACKUP_ROOT/deal-hunter.skill.md.bak"
		else
			echo "[WARN] 备份 $target 失败，先停在这里：不覆盖它" >&2
			exit 1
		fi
	fi
	install -m 0644 "$SRC_DIR/deal-hunter.skill.md" "$target"
	echo "[OK] 技能说明已放到 $target"
else
	echo "[INFO] 未发现标准 skills 目录，仅安装到 workspace。"
fi

echo
echo "[DONE] 验证："
echo "  bash $WS/dealhunter-status.sh"
echo "  bash $WS/dealhunter-deals.sh 70 5"
echo "  bash $WS/dealhunter-digest.sh"
