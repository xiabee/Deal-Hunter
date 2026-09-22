#!/usr/bin/env bash
# Back up Deal-Hunter's state and config, then prove the archive restores.
#
#   sudo ./deploy/backup.sh                          # -> /var/backups/deal-hunter
#   DH_BACKUP_DIR=/mnt/nas/dh sudo ./deploy/backup.sh
#   DH_BACKUP_PUSH=other-host:/backups/dh sudo ./deploy/backup.sh   # copy off this disk
#   DH_BACKUP_KEEP=14 sudo ./deploy/backup.sh
#   DH_BACKUP_ROOT=/tmp/sandbox ./deploy/backup.sh   # 假根目录演练用；不需要 root
#
# The archive is a plain tar of /var/lib/deal-hunter and /etc/deal-hunter, so
# restoring needs no tooling:
#
#   sudo systemctl stop deal-hunter
#   sudo tar xzf deal-hunter-<stamp>.tar.gz -C /
#   sudo chown -R dealhunter:dealhunter /var/lib/deal-hunter
#   sudo systemctl start deal-hunter
#
# A backup that was never opened is a wish, so the script extracts it into a
# throwaway directory and compares row counts against the live files before it
# reports success.
set -euo pipefail

# Everything hangs off one root so the same script can be pointed at a sandbox tree
# with the production layout intact - the archive keeps containing var/lib/... and
# etc/..., which is what makes the drill worth running.
ROOT="${DH_BACKUP_ROOT:-/}"
PROD_ROOT=1
[[ $ROOT == "/" ]] || PROD_ROOT=0
ROOT="${ROOT%/}"
STATE="$ROOT/var/lib/deal-hunter"
ETC="$ROOT/etc/deal-hunter"
DIR="${DH_BACKUP_DIR:-$ROOT/var/backups/deal-hunter}"
KEEP="${DH_BACKUP_KEEP:-14}"
PUSH="${DH_BACKUP_PUSH:-}"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
NAME="deal-hunter-$STAMP"

log() { printf '\033[1;36m▸\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

[[ $STATE == /* && $ETC == /* && $DIR == /* ]] || die "路径必须是绝对路径（DH_BACKUP_ROOT=$ROOT）"

# Retention prunes by deleting. A KEEP of 0 makes `head -n -0` list *every* archive,
# so a mistyped unit would empty the backup directory in one run; a non-number just
# silently stops pruning. Refuse both instead.
[[ $KEEP =~ ^[0-9]+$ ]] || die "DH_BACKUP_KEEP 必须是非负整数，收到的是「$KEEP」"
(( KEEP >= 1 )) || die "DH_BACKUP_KEEP=0 会把 $DIR 里的归档全删光；至少留 1 份"

if [[ $PROD_ROOT == 1 ]]; then
	[[ $EUID -eq 0 ]] || die "需要 root（状态目录是 0750 dealhunter）：sudo $0"
else
	[[ $EUID -eq 0 ]] || log "以非 root 身份跑在假根 ${DH_BACKUP_ROOT} 下（演练模式）"
fi
[[ -d $STATE ]] || die "找不到 $STATE"

# The archive contains /etc/deal-hunter/deal-hunter.env, i.e. the webhook and its
# signing secret. Default permissions on those files are 0600/0640 and a tarball
# must not be the one place that widens them.
umask 077
if [[ $EUID -eq 0 ]]; then
	install -d -m 0750 -o root -g root "$DIR"
else
	install -d -m 0750 "$DIR"
fi
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Upgrade backups are excluded: they are byte-for-byte copies of a config that
# changed, and keeping them makes every archive grow without adding recovery
# value beyond the last one.
tar --create --gzip --file "$DIR/$NAME.tar.gz" \
	--exclude='*.bak-*' --directory="${ROOT:-/}" var/lib/deal-hunter etc/deal-hunter \
	2> >(grep -v 'file changed as we read it' >&2 || true)
# A collection round may write while we read; tar warns rather than fails, and
# the restore check below is what decides whether the archive is usable.

( cd "$DIR" && sha256sum "$NAME.tar.gz" > "$NAME.tar.gz.sha256" )
# The list file holds a bare name, so the check has to run from that directory.
( cd "$DIR" && sha256sum --quiet --check "$NAME.tar.gz.sha256" ) || die "校验和不对，归档已损坏"

tar -xzf "$DIR/$NAME.tar.gz" -C "$TMP"
live_deals=$(wc -l < "$STATE/deals.jsonl" 2>/dev/null || echo 0)
back_deals=$(wc -l < "$TMP/var/lib/deal-hunter/deals.jsonl" 2>/dev/null || echo -1)
[[ -f "$TMP/etc/deal-hunter/config.json" ]] || die "归档里没有 config.json"
if (( back_deals > live_deals )); then
	die "归档比在线文件还新（$back_deals > $live_deals），本轮不采信"
fi
log "归档可解开：$back_deals/$live_deals 行情位，config.json 在位"
if (( back_deals < live_deals )); then
	warn "少 $((live_deals - back_deals)) 行：采集轮正好在写入，下一次备份会更全"
fi

if [[ -n $PUSH ]]; then
	scp -q "$DIR/$NAME.tar.gz" "$DIR/$NAME.tar.gz.sha256" "$PUSH/" \
		&& log "已另存一份到 $PUSH" \
		|| die "推送到 $PUSH 失败（本机这份仍在）"
fi

# Prune by modification time, keeping the newest $KEEP pairs. Deliberately not by
# name: the name is a convenience, and the moment the stamp format differs from what
# is already on disk, a name-sorted list puts the *newest* archive first and
# retention deletes the one backup we can actually trust.
mapfile -t old < <(cd "$DIR" && ls -1t deal-hunter-*.tar.gz 2>/dev/null | tail -n +"$((KEEP + 1))")
for f in "${old[@]:-}"; do
	[[ -n $f ]] || continue
	rm -f "$DIR/$f" "$DIR/$f.sha256"
	log "轮换掉 $f"
done

# Leave a record the service account can read, so `dealhunter doctor` can answer
# "when did backups actually last succeed" - a timer that keeps failing only speaks
# through the journal, which nobody reads. Contents are a timestamp and a filename,
# so 0640 is enough and nothing secret lands here.
if printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$NAME.tar.gz" > "$STATE/.backup.stamp.tmp" 2>/dev/null; then
	chmod 0640 "$STATE/.backup.stamp.tmp" 2>/dev/null || true
	chown --reference="$STATE" "$STATE/.backup.stamp.tmp" 2>/dev/null || true
	mv -f "$STATE/.backup.stamp.tmp" "$STATE/backup.stamp" 2>/dev/null ||
		warn "写 $STATE/backup.stamp 失败：doctor 会一直报「没有备份记录」，备份本身是好的"
fi

log "备份完成：$DIR/$NAME.tar.gz ($(du -h "$DIR/$NAME.tar.gz" | cut -f1))，保留最近 $KEEP 份"
