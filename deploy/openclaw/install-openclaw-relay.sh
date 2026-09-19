#!/usr/bin/env bash
# Wire Deal-Hunter's delivery through an existing OpenClaw installation, so the
# radar reuses the assistant's already-connected Feishu session instead of
# creating a second bot.
#
# Privilege model (the point of this script):
#   · The gateway token is read ONCE from the running gateway's own process env
#     and stored in a root-only 0600 file. It is never printed, never committed,
#     and never placed in the Deal-Hunter service user's environment.
#   · The service user gets exactly one sudoers-granted capability: running the
#     wrapper below, which only forwards "message send" to OpenClaw as the
#     openclaw user.
#
# Usage (run on the target host, as root or with sudo available):
#   sudo ./deploy/openclaw/install-openclaw-relay.sh --target <feishu-chat-or-user-id>
#   sudo ./deploy/openclaw/install-openclaw-relay.sh --target ou_xxxx --dashboard-bind 100.x.x.x:8765
#
# Flags:
#   --target <id>          Feishu recipient for `openclaw message send -t` (required)
#   --channel <name>       OpenClaw channel (default: feishu)
#   --config <path>        Deal-Hunter config to update (default: /etc/deal-hunter/config.json)
#   --env <path>           service env file (default: /etc/deal-hunter/deal-hunter.env)
#   --service-user <name>  systemd service account (default: dealhunter)
#   --openclaw-user <name> account owning the gateway (default: openclaw)
#   --gateway-port <port>  local gateway port used to locate the process (default: 18789)
#   --dashboard-bind <a>   optional server.bind override for the read-only panel
#   --skip-send            write config only, do not probe the wrapper
set -euo pipefail

TARGET="" CHANNEL="feishu" CONFIG="/etc/deal-hunter/config.json"
ENVF="/etc/deal-hunter/deal-hunter.env" SVC_USER="dealhunter" OC_USER="openclaw"
GW_PORT="18789" DASH_BIND="" SKIP_SEND=0
WRAP=/usr/local/bin/deal-hunter-openclaw-send
TOKEN_FILE=/etc/deal-hunter/openclaw-gateway.token
SUDOERS=/etc/sudoers.d/deal-hunter

while [ $# -gt 0 ]; do
	case "$1" in
		--target) TARGET="${2:-}"; shift 2 ;;
		--channel) CHANNEL="${2:-}"; shift 2 ;;
		--config) CONFIG="${2:-}"; shift 2 ;;
		--env) ENVF="${2:-}"; shift 2 ;;
		--service-user) SVC_USER="${2:-}"; shift 2 ;;
		--openclaw-user) OC_USER="${2:-}"; shift 2 ;;
		--gateway-port) GW_PORT="${2:-}"; shift 2 ;;
		--dashboard-bind) DASH_BIND="${2:-}"; shift 2 ;;
		--skip-send) SKIP_SEND=1; shift ;;
		-h|--help) sed -n '1,30p' "$0"; exit 0 ;;
		*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

log() { printf '\033[1;36m▸\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

[ -n "$TARGET" ] || die "--target is required (find it with: openclaw directory peers list --channel $channel)"
[ "$(id -u)" -eq 0 ] || die "run me as root (sudo ./deploy/openclaw/install-openclaw-relay.sh --target ...)"
[ -f "$CONFIG" ] || die "no config at $CONFIG — run deploy/install.sh first"
command -v python3 >/dev/null || die "python3 is required to edit the config safely"

log "locating the running gateway (port $GW_PORT)"
# Match the gateway process itself. Port-based lookup is unreliable because a
# mesh daemon (tailscaled) may forward the same port number.
GW_PID=$(pgrep -f "openclaw/dist/index.js gateway" | head -1 || true)
[ -n "$GW_PID" ] || GW_PID=$(pgrep -f "index.js gateway --port $GW_PORT" | head -1 || true)
[ -n "$GW_PID" ] || GW_PID=$(pgrep -f "openclaw.*gateway" | head -1 || true)
[ -n "$GW_PID" ] || die "no gateway process found — start OpenClaw first"
OC_PID_USER=$(ps -o user= -p "$GW_PID" | tr -d ' ')
log "gateway pid=$GW_PID user=$OC_PID_USER"

if [ -s "$TOKEN_FILE" ]; then
	log "reusing the existing token file (re-extraction skipped)"
else
	log "extracting OPENCLAW_GATEWAY_TOKEN from the gateway process env (value never printed)"
	TOK=$(tr '\0' '\n' < "/proc/$GW_PID/environ" | sed -n "s/^OPENCLAW_GATEWAY_TOKEN=//p" | head -1) || true
	[ -n "${TOK:-}" ] || die "the gateway does not expose OPENCLAW_GATEWAY_TOKEN in its environment;
  supply it another way and write it to $TOKEN_FILE (root:root 0600)"
	umask 077
	printf '%s' "$TOK" > "$TOKEN_FILE"
	unset TOK
fi
chown "root:$OC_PID_USER" "$TOKEN_FILE"; chmod 0640 "$TOKEN_FILE"
log "token stored at $TOKEN_FILE ($(wc -c < "$TOKEN_FILE") bytes, readable by root and $OC_PID_USER only)"

log "writing the send-only wrapper at $WRAP"
cat > "$WRAP" <<'WRAPPER'
#!/usr/bin/env bash
# Root-only helper for the dealhunter service account. Exposes exactly one
# capability: forwarding "message send" to OpenClaw as the openclaw user.
# The gateway token lives in a root-owned file and is injected only into the
# child, so neither the service user nor the repo ever sees it.
set -euo pipefail

if [ "$#" -lt 4 ] || [ "${1:-}" != "message" ] || [ "${2:-}" != "send" ]; then
  echo "refused: expected 'message send --channel <c> --target <t> --message <m>'" >&2
  exit 64
fi

# Only the flags Deal-Hunter needs. Anything able to read local files or change
# delivery semantics is refused outright. Validation never consumes "$@", so the
# original argv reaches OpenClaw untouched.
allow=' --channel --target --message --account -m -t --dry-run --json --reply-to --thread-id '
reject='--media --presentation --delivery --force-document --gif-playback --silent --pin'
for a in "$@"; do
  case "$a" in
    -*)
      key="${a%%=*}"
      case " $reject " in *" $key "*) echo "refused: $key is not permitted" >&2; exit 64 ;; esac
      case "$allow" in *" $key "*) ;; *) echo "refused: unknown flag $key" >&2; exit 64 ;; esac
      ;;
  esac
done

TOKEN_FILE=/etc/deal-hunter/openclaw-gateway.token
[ -r "$TOKEN_FILE" ] || { echo "refused: $TOKEN_FILE unreadable" >&2; exit 66; }

# The token is read by the child shell, never passed in argv (that would show it
# in `ps`) and never via sudo --preserve-env (which needs a setenv allowance).
# The file is group-readable by the gateway user only; the service user is not
# in that group and therefore cannot read it directly.
exec /usr/bin/sudo -n -u openclaw env HOME=/home/openclaw \
  DH_TOKEN_FILE="$TOKEN_FILE" /bin/sh -c \
  'OPENCLAW_GATEWAY_TOKEN="$(cat "$DH_TOKEN_FILE")"; export OPENCLAW_GATEWAY_TOKEN; exec /usr/local/bin/openclaw "$@"' sh "$@"
WRAPPER
chown root:root "$WRAP"; chmod 0755 "$WRAP"
# The gateway may be owned by a differently-named account; follow the process.
if [ "$OC_PID_USER" != "openclaw" ]; then
	sed -i "s/-u openclaw /-u $OC_PID_USER /" "$WRAP"
	sed -i "s#HOME=/home/openclaw#HOME=/home/$OC_PID_USER#" "$WRAP"
	log "wrapper retargeted to the gateway owner ($OC_PID_USER)"
fi
bash -n "$WRAP" || die "generated wrapper is not valid bash"

log "installing the sudoers rule"
if ! id "$SVC_USER" >/dev/null 2>&1; then die "service user $SVC_USER does not exist"; fi
printf '%s ALL=(root) NOPASSWD: %s\n' "$SVC_USER" "$WRAP" > "$SUDOERS"
chown root:root "$SUDOERS"; chmod 0440 "$SUDOERS"
visudo -cf "$SUDOERS" >/dev/null || die "sudoers syntax rejected"

log "enabling the relay in $CONFIG"
CONFIG="$CONFIG" CHANNEL="$CHANNEL" WRAP="$WRAP" python3 - <<'PY'
import json, os
path = os.environ["CONFIG"]
with open(path) as f:
    cfg = json.load(f)
oc = cfg.setdefault("notify", {}).setdefault("openclaw", {})
oc["enabled"] = True
oc["relay"] = True
oc["channel"] = os.environ["CHANNEL"]
oc["command"] = "/usr/bin/sudo"
oc["args"] = ["-n", os.environ["WRAP"]]
backup = path + ".bak-relay"
if not os.path.exists(backup):
    with open(path) as src, open(backup, "w") as dst:
        dst.write(src.read())
with open(path, "w") as f:
    json.dump(cfg, f, indent=2, ensure_ascii=False)
    f.write("\n")
print("[ok] notify.openclaw.relay = true")
PY

log "recording the recipient in $ENVF"
if [ -f "$ENVF" ]; then
	grep -q "^DH_OPENCLAW_TARGET=" "$ENVF" || printf 'DH_OPENCLAW_TARGET=\n' >> "$ENVF"
	sed -i \
		-e "s|^DH_OPENCLAW_TARGET=.*|DH_OPENCLAW_TARGET=$TARGET|" \
		-e "s|^DH_OPENCLAW_CHANNEL=.*|DH_OPENCLAW_CHANNEL=$CHANNEL|" \
		-e "s|^DH_OPENCLAW_RELAY=.*|DH_OPENCLAW_RELAY=1|" \
		"$ENVF"
	[ -n "$DASH_BIND" ] && sed -i -e "s|^DH_SERVER_BIND=.*|DH_SERVER_BIND=$DASH_BIND|" "$ENVF"
	chown "root:$SVC_USER" "$ENVF" 2>/dev/null || chown root "$ENVF"
	chmod 0640 "$ENVF"
	log "env updated (target recorded, relay on)$( [ -n "$DASH_BIND" ] && echo ', dashboard rebind requested' )"
else
	log "no env file at $ENVF — set DH_OPENCLAW_TARGET=$TARGET there yourself"
fi

if [ "$SKIP_SEND" -eq 1 ]; then log "skipping the live probe"; exit 0; fi

log "probing the wrapper as $SVC_USER (dry-run, nothing delivered)"
if ! sudo -n -u "$SVC_USER" test -x "$WRAP"; then die "service user cannot see $WRAP"; fi
OUT=$(sudo -n -u "$SVC_USER" /usr/bin/sudo -n "$WRAP" message send \
	--channel "$CHANNEL" --target "$TARGET" --message "probe" --dry-run 2>&1 | tail -4) || true
printf '%s\n' "$OUT"
case "$OUT" in
	*"GATEWAY_SECRET_REF_UNAVAILABLE"*|*refused*) die "the relay path is not usable yet — see the message above" ;;
esac

if command -v systemctl >/dev/null; then
	log "restarting the service"
	systemctl restart deal-hunter 2>/dev/null || log "could not restart deal-hunter (is the unit installed?)"
fi

cat <<DONE

✅ OpenClaw 中转已就绪。验证真实投递：
   sudo -u $SVC_USER /opt/deal-hunter/deal-hunter notify-test -config $CONFIG
   journalctl -u deal-hunter -n 8 --no-pager
换收件人：编辑 $ENVF 的 DH_OPENCLAW_TARGET（群聊用 oc_ 开头的 chat id，
可用 openclaw directory groups list --channel $CHANNEL 查询）。
DONE
