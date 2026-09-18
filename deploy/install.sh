#!/usr/bin/env bash
# Install or upgrade Deal-Hunter on a Linux host as a hardened systemd service.
#
#   sudo dist/dealhunter-linux-amd64=... ./deploy/install.sh [path-to-binary]
#   sudo ./deploy/install.sh dist/dealhunter-linux-amd64
#
# The target needs no Go toolchain: build the binary with `make build-linux`
# (locally or on the office CI builder) and ship it. Re-running is safe:
# existing files are kept as timestamped backups and never overwritten blindly.
set -euo pipefail

BIN_SRC="${1:-dist/dealhunter-linux-amd64}"
PREFIX=/opt/deal-hunter
ETC=/etc/deal-hunter
STATE=/var/lib/deal-hunter
SERVICE=deal-hunter
RUNUSER=dealhunter
STAMP="$(date +%Y%m%d-%H%M%S)"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\033[1;36m▸\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "请以 root 运行：sudo ./deploy/install.sh $BIN_SRC"
[[ -f "$BIN_SRC" ]] || die "找不到二进制 $BIN_SRC，请先执行 make build-linux"
command -v systemctl >/dev/null || die "需要 systemd"

# The binary must run on this machine; a wrong-arch artifact fails loudly here.
if ! "$BIN_SRC" version >/dev/null 2>&1; then
	die "$BIN_SRC 无法在本机执行（架构不匹配？用 make build-linux 或 GOARCH=arm64 重新编译）"
fi
log "安装源：$("$BIN_SRC" version)"

log "创建服务账户 $RUNUSER"
id -u "$RUNUSER" >/dev/null 2>&1 || useradd --system --home "$STATE" --shell /usr/sbin/nologin "$RUNUSER"

install -d -m 0755 -o root -g root "$PREFIX"
install -d -m 0750 -o "$RUNUSER" -g "$RUNUSER" "$STATE"
install -d -m 0755 -o root -g root "$ETC"

if [[ -f "$PREFIX/deal-hunter" ]]; then
	cp -a "$PREFIX/deal-hunter" "$PREFIX/deal-hunter.bak-$STAMP"
	log "旧版本已备份为 deal-hunter.bak-$STAMP"
fi
install -m 0755 "$BIN_SRC" "$PREFIX/deal-hunter"

if [[ ! -f "$ETC/config.json" ]]; then
	install -m 0644 "$REPO_ROOT/config/deal-hunter.example.json" "$ETC/config.json"
	log "已放置默认配置 $ETC/config.json（可直接编辑，信息源按需增删）"
else
	cp -a "$ETC/config.json" "$ETC/config.json.bak-$STAMP"
	log "保留现有配置，备份为 config.json.bak-$STAMP"
fi

if [[ ! -f "$ETC/deal-hunter.env" ]]; then
	install -m 0600 "$REPO_ROOT/deploy/deal-hunter.env.example" "$ETC/deal-hunter.env"
	chown root:"$RUNUSER" "$ETC/deal-hunter.env"
	warn "请填写 $ETC/deal-hunter.env 里的 DH_FEISHU_WEBHOOK / DH_FEISHU_SECRET（当前是占位符）"
fi

install -m 0644 "$REPO_ROOT/deploy/deal-hunter.service" "/etc/systemd/system/$SERVICE.service"

log "systemd 重载并启动"
systemctl daemon-reload
systemctl enable "$SERVICE.service" >/dev/null 2>&1
systemctl restart "$SERVICE.service"

sleep 2
if systemctl is-active --quiet "$SERVICE.service"; then
	log "服务已运行"
else
	warn "服务未就绪，最近的日志："
	journalctl -u "$SERVICE" -n 25 --no-pager || true
fi

echo
echo "下一步："
echo "  1) 编辑 $ETC/deal-hunter.env，填入飞书自定义机器人的 webhook 与签名密钥"
echo "  2) sudo systemctl restart $SERVICE && sudo -u $RUNUSER $PREFIX/deal-hunter notify-test"
echo "  3) 查看状态：systemctl status $SERVICE --no-pager；journalctl -u $SERVICE -f"
echo "  4) 只读面板：curl -s http://127.0.0.1:8765/api/v1/status"
echo "  5) 可选：sudo -u <openclaw-user> ./deploy/openclaw/install-openclaw-integration.sh"
