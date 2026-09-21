#!/usr/bin/env bash
# Run the full Deal-Hunter gate on an office builder over SSH, consuming zero
# GitHub Actions minutes. The working tree is shipped as a verified tarball and
# scripts/ci-local.sh runs there; the builder only ever sees non-ignored files,
# so local .env secrets are never transmitted.
#
# Usage:
#   DH_CI_HOST=<ssh-alias> scripts/ci-office.sh            # full gate
#   DH_CI_HOST=<ssh-alias> scripts/ci-office.sh --quick    # skip race+cross
#
# The host needs a Go toolchain (>= 1.24) and GNU/BSD tar. Reaching it over a
# private mesh (e.g. Tailscale) is expected; nothing is exposed publicly.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

HOST="${DH_CI_HOST:-}"
MODE="${1:-}"
if [[ -z "$HOST" ]]; then
	cat >&2 <<'USAGE'
DH_CI_HOST is required: the ssh alias or address of a trusted office builder.
Example:
  DH_CI_HOST=builder.local scripts/ci-office.sh
USAGE
	exit 2
fi

REMOTE_DIR="deal-hunter-ci-$(date +%s)-$$"
LIST=""
TARBALL=""
cleanup() { [[ -n "$LIST" ]] && rm -f "$LIST"; [[ -n "$TARBALL" ]] && rm -f "$TARBALL"; }
trap cleanup EXIT
keep_remote="${DH_CI_KEEP:-0}"

say() { printf '\033[1;36m▸\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

say "preflight: ssh $HOST"
ssh -o BatchMode=yes -o ConnectTimeout=10 "$HOST" \
	'command -v go >/dev/null || { echo "go toolchain missing on the builder" >&2; exit 3; }; go version; mkdir -p "/tmp/'"$REMOTE_DIR"'"' \
	|| die "cannot prepare the builder at $HOST"

say "packing the working tree (tracked + new, ignoring gitignored paths)"
LIST="$(mktemp)"
TARBALL="$(mktemp)"
git ls-files --cached --others --exclude-standard -z > "$LIST"
# --no-recursion: the file list already enumerates every path.
tar --null --no-recursion --files-from="$LIST" -cf "$TARBALL" || die "tar failed"

FILES_SHIPPED="$(tr '\0' '\n' < "$LIST" | grep -c . || true)"
BYTES="$(wc -c < "$TARBALL" | tr -d ' ')"
[[ "$FILES_SHIPPED" -gt 30 ]] || die "only $FILES_SHIPPED files listed — is git available?"
# Read the listing into a variable rather than piping tar into grep: grep -q exits at
# the first match, which SIGPIPEs tar, and under pipefail that 141 looks exactly like a
# missing file - a gate that fails its own healthy payload.
LISTING="$(tar -tf "$TARBALL" 2>/dev/null)" || die "cannot read back the payload"
grep -qx 'go.mod' <<<"$LISTING" || die "go.mod missing from the payload"
grep -qx 'scripts/ci-local.sh' <<<"$LISTING" || die "ci script missing from the payload"
if grep -qE '(^|/)[^/]*\.env$' <<<"$LISTING"; then
	die "refusing to ship a .env file to the builder"
fi
say "payload: $FILES_SHIPPED files, $BYTES bytes"

say "uploading to $HOST:/tmp/$REMOTE_DIR"
cat "$TARBALL" | ssh -o BatchMode=yes "$HOST" "tar -xf - -C /tmp/$REMOTE_DIR" || die "upload/extract failed"
ssh -o BatchMode=yes "$HOST" "test -f /tmp/$REMOTE_DIR/go.mod && test -f /tmp/$REMOTE_DIR/scripts/ci-local.sh" \
	|| die "payload did not land intact"

say "running scripts/ci-local.sh on $HOST ${MODE:+($MODE)}"
if ssh -o BatchMode=yes "$HOST" "cd /tmp/$REMOTE_DIR && bash scripts/ci-local.sh $MODE"; then
	RC=0
else
	RC=$?
fi

if [[ "$RC" -eq 0 ]]; then
	if [[ "$keep_remote" == "1" ]]; then
		say "keeping the builder workspace as requested"
	else
		ssh -o BatchMode=yes "$HOST" "rm -rf /tmp/$REMOTE_DIR" || true
	fi
	echo "✓ office CI passed ($HOST, $FILES_SHIPPED files)"
	exit 0
fi

printf '\033[1;31m✗ office CI failed (rc=%s) on %s\033[0m\n' "$RC" "$HOST" >&2
echo "  builder workspace kept for debugging: /tmp/$REMOTE_DIR" >&2
exit "$RC"
