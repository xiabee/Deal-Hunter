#!/usr/bin/env bash
# Install or upgrade Deal-Hunter on a Linux host as a hardened systemd service.
#
#   sudo dist/dealhunter-linux-amd64=... ./deploy/install.sh [path-to-binary]
#   sudo ./deploy/install.sh dist/dealhunter-linux-amd64
#
# The target needs no Go toolchain: build the binary with `make build-linux`
# (locally or on the office CI builder) and ship it. Re-running is safe: the live
# config is never overwritten, and what it replaces is moved aside first - one
# previous binary, and one config snapshot per day.
set -euo pipefail

BIN_SRC="${1:-dist/dealhunter-linux-amd64}"
PREFIX=/opt/deal-hunter
ETC=/etc/deal-hunter
STATE=/var/lib/deal-hunter
SERVICE=deal-hunter
RUNUSER=dealhunter
DAY="$(date +%Y%m%d)"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\033[1;36m▸\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "请以 root 运行：sudo ./deploy/install.sh $BIN_SRC"
[[ -f "$BIN_SRC" ]] || die "找不到二进制 $BIN_SRC，请先执行 make build-linux"
command -v systemctl >/dev/null || die "需要 systemd"

# The binary must run on this machine; a wrong-arch or non-executable artifact
# fails here with the real reason rather than a guess.
chmod +x "$BIN_SRC" 2>/dev/null || true
# "./x" and "x" are the same file to a human and different things to bash.
[[ $BIN_SRC == */* ]] || BIN_SRC="./$BIN_SRC"
if ! ver_out="$("$BIN_SRC" version 2>&1)"; then
	echo "$ver_out" >&2
	die "$BIN_SRC 无法在本机执行（上面是系统给出的原因；如是架构问题，用 GOARCH=arm64 重新编译）"
fi
log "安装源：$ver_out"

log "创建服务账户 $RUNUSER"
id -u "$RUNUSER" >/dev/null 2>&1 || useradd --system --home "$STATE" --shell /usr/sbin/nologin "$RUNUSER"

install -d -m 0755 -o root -g root "$PREFIX"
install -d -m 0750 -o "$RUNUSER" -g "$RUNUSER" "$STATE"
install -d -m 0755 -o root -g root "$ETC"

# One previous binary under a fixed name. The old shape copied the running binary
# aside under a fresh timestamp on every upgrade and nothing reclaimed them: 8.5 MB
# each, 28 on the machine that runs this service, 227 MB for a rollback convenience
# that is a lie anyway - further back than one version you would rebuild from git.
# "Which version is this" does not need a filename, `./deal-hunter.bak-prev version`
# answers it from the build stamp inside. The .bak- prefix is load-bearing: that is
# what keeps these out of every archive backup.sh writes.
if [[ -f "$PREFIX/deal-hunter" ]]; then
	mv -f "$PREFIX/deal-hunter" "$PREFIX/deal-hunter.bak-prev"
	log "旧版本已挪为 deal-hunter.bak-prev（只留上一版，再往前从 git 重建）"
fi
install -m 0755 "$BIN_SRC" "$PREFIX/deal-hunter"
install -m 0755 "$REPO_ROOT/deploy/backup.sh" "$PREFIX/backup.sh"
install -m 0755 "$REPO_ROOT/deploy/wait-healthy.sh" "$PREFIX/wait-healthy.sh"
install -m 0644 "$REPO_ROOT/deploy/deal-hunter-backup.service" "/etc/systemd/system/deal-hunter-backup.service"
install -m 0644 "$REPO_ROOT/deploy/deal-hunter-backup.timer" "/etc/systemd/system/deal-hunter-backup.timer"
# Deliberately not enabled: turning on a recurring job that writes outside the
# project's own directory should be a decision, not a side effect of upgrading.
log "备份脚本已就位（$PREFIX/backup.sh）；要每晚自动备份：sudo systemctl enable --now deal-hunter-backup.timer"

if [[ ! -f "$ETC/config.json" ]]; then
	# The starter config omits "sources" on purpose so the built-in collector set
	# stays active; the fully-commented example is installed alongside as a guide.
	install -m 0644 "$REPO_ROOT/config/deal-hunter.starter.json" "$ETC/config.json"
	install -m 0644 "$REPO_ROOT/config/deal-hunter.example.json" "$ETC/config.example.json"
	log "已放置 $ETC/config.json（沿用内置 15 个信息源），参考 $ETC/config.example.json"
else
	# A snapshot per day rather than per run. Deploys come in tens on a working day,
	# and each one used to leave a file in the directory that also holds the webhook
	# secret. Production config is deliberately not in git, so this history is the
	# only copy of it - keep it, just not 28 copies of the same afternoon.
	cp -a "$ETC/config.json" "$ETC/config.json.bak-$DAY"
	log "保留现有配置，今天的旧副本已更新为 config.json.bak-$DAY"
	log "注意：本版交付形态改为每天一份日报。旧配置里的 notify.digest、notify.feishu.{min_score,max_per_run,silent_hours} 已废弃——留着不报错，但不再生效；filter.min_score 与 DH_MIN_SCORE 从未参与过任何过滤，已一并删除。可对照仓库里的 config/deal-hunter.example.json 清理，原配置已备份。"
fi

# Hosts upgraded from the per-run scheme still have those files on disk. Deleting
# them is whoever owns the host's call, but it should not be invisible.
shopt -s nullglob
legacy_bin=("$PREFIX"/deal-hunter.bak-[0-9]*)
legacy_cfg=("$ETC"/config.json.bak-[0-9]*-[0-9]*)
shopt -u nullglob
if (( ${#legacy_bin[@]} + ${#legacy_cfg[@]} > 0 )); then
	warn "盘上还有旧式逐次备份：$PREFIX 里 ${#legacy_bin[@]} 个二进制、$ETC 里 ${#legacy_cfg[@]} 个配置（新的排法不再产生它们）。要清就一条：sudo rm -f $PREFIX/deal-hunter.bak-[0-9]* $ETC/config.json.bak-[0-9]*-[0-9]*"
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

# "systemd said active" is not "the service works". The unit is hardened and the
# state directory is root-owned, so the ways this can go wrong are the quiet ones:
# a config that validates and then refuses to load, a bind another process holds,
# a crash loop that is active for exactly one poll. So the acceptance check is the
# panel answering /healthz, and a failure here fails the install.
BIND=""
PANEL_ON=1
if [[ -f "$ETC/deal-hunter.env" ]]; then
	# The env file is shell syntax sourced by systemd, so a quoted value has to be
	# unquoted here too - and a trailing CR would end up inside the URL.
	BIND="$(sed -n 's/^DH_SERVER_BIND=//p' "$ETC/deal-hunter.env" | tail -1 | tr -d '\r' | sed -e 's/^"//' -e 's/"$//')"
fi
if [[ -f "$ETC/config.json" ]]; then
	config_bind="$(sed -n 's@.*"bind"[[:space:]]*:[[:space:]]*"\([^"]*\)".*@\1@p' "$ETC/config.json" | head -1)"
	if grep -q '"enabled"[[:space:]]*:[[:space:]]*false' "$ETC/config.json"; then
		PANEL_ON=0
	fi
	[[ -n "$BIND" ]] || BIND="$config_bind"
fi
if [[ "$PANEL_ON" != "1" ]]; then
	warn "面板在配置里是关的，健康检查跳过（服务本身仍在跑采集）"
elif [[ -z "$BIND" ]]; then
	die "读不到面板监听地址（$ETC/deal-hunter.env 的 DH_SERVER_BIND 或 config.json 的 server.bind），无法确认服务是否真的在服务"
else
	# env wins over config.json at runtime, so the same precedence has to be probed.
	if ! "$PREFIX/wait-healthy.sh" "http://$BIND" 30; then
		warn "服务未就绪，最近的日志："
		journalctl -u "$SERVICE" -n 25 --no-pager || true
		die "重启后 $BIND/healthz 没有答 200：安装判为失败，而不是「装好了但没起来」"
	fi
	log "服务已运行，面板在 $BIND 上应答 200"
fi

echo
echo "下一步："
echo "  1) 编辑 $ETC/deal-hunter.env，填入飞书自定义机器人的 webhook 与签名密钥"
echo "  2) sudo systemctl restart $SERVICE && sudo -u $RUNUSER $PREFIX/deal-hunter notify-test"
echo "  3) 查看状态：systemctl status $SERVICE --no-pager；journalctl -u $SERVICE -f"
echo "  4) 只读面板：curl -s http://127.0.0.1:8765/api/v1/status"
echo "  5) 可选：sudo -u <openclaw-user> ./deploy/openclaw/install-openclaw-integration.sh"
