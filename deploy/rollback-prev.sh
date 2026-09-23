#!/usr/bin/env bash
# Put the previous version back and restart, so a failed upgrade does not leave the
# host running (or trying to run) the binary that just failed.
#
#   deploy/rollback-prev.sh            # /opt/deal-hunter, restarts the service
#   DH_DEPLOY_PREFIX=/tmp/x deploy/rollback-prev.sh
#
# install.sh calls this when the panel never answers after a restart; it is also the
# command an operator runs by hand after deciding a new build is the bad one. The
# prefix is overridable for the same reason backup.sh takes DH_BACKUP_ROOT: the
# sandbox is the only place a destructive move can be exercised without a host.
#
# Deliberately does not keep the rejected binary: it is still sitting at whatever
# path install.sh was handed, and the point of this script is that the *live* path
# goes back to something that answered. Keeping a copy under /opt is how the old
# install flow accumulated 27 binaries.
set -euo pipefail

PREFIX="${DH_DEPLOY_PREFIX:-/opt/deal-hunter}"
BIN="$PREFIX/deal-hunter"
PREV="$PREFIX/deal-hunter.bak-prev"
SERVICE="${DH_SERVICE_NAME:-deal-hunter}"

die() { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }
log() { printf '\033[1;36m▸\033[0m %s\n' "$*"; }

[[ -f "$PREV" ]] || die "$PREV 不存在：这台机器上没有可退回的上一版（首次安装失败只能直接修，别退）"
[[ -x "$PREV" ]] || die "$PREV 不可执行，退回它只会换一个起不来的版本"

# "Which version is this" is answered by the file itself, not by a filename - that
# is the whole reason the previous copy carries a fixed name.
prev_version="$("$PREV" version 2>&1)" || die "$PREV 跑不起来，不退回：$prev_version"
log "退回上一版：$prev_version"

mv -f "$PREV" "$BIN"
log "$BIN 已换回上一版（那份失败的产物还在 install.sh 收到的路径上，没有另存副本）"

if ! command -v systemctl >/dev/null 2>&1; then
	die "没有 systemctl，已换回二进制但没能重启：自己跑 systemctl restart $SERVICE"
fi
systemctl restart "$SERVICE"
log "已重启 $SERVICE"
