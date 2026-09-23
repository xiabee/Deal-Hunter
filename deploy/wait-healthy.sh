#!/usr/bin/env bash
# Wait until a Deal-Hunter listener answers /healthz, then say which one.
#
#   deploy/wait-healthy.sh <http://host:port> [timeout-seconds]
#
# install.sh runs this after (re)starting the service, and treats a failure as a
# failed install. Without it, "installer rc=0" only meant systemd had said
# `active` once - a process that answers for a second and then crash-loops on a
# bad config looked installed.
#
# Exit codes: 0 healthy, 1 never became healthy, 2 the arguments were wrong.
set -euo pipefail

URL="${1:-}"
WAIT="${2:-30}"

if [[ -z "$URL" ]]; then
	echo "用法：deploy/wait-healthy.sh <http://host:port> [秒数]" >&2
	exit 2
fi
URL="${URL%/}"
case "$WAIT" in
'' | *[!0-9]*) echo "等待秒数必须是整数，收到的是「$WAIT」" >&2; exit 2 ;;
esac
[[ "$URL" == http://* ]] || { echo "地址必须是 http://…，收到的是「$URL」" >&2; exit 2; }

deadline=$((SECONDS + WAIT))
last=""
while :; do
	# A client that never got a response reports nothing at all; that is a
	# different fact from "404", and folding the two together would hide a dead
	# listener behind a wrong status code.
	code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$URL/healthz" 2>/dev/null || true)"
	if [[ "$code" == "200" ]]; then
		echo "healthy: $URL/healthz = 200（等待 ${SECONDS}s）"
		exit 0
	fi
	last="$code"
	((SECONDS < deadline)) || break
	sleep 0.5
done

if [[ -z "$last" || "$last" == "000" || "$last" != [0-9][0-9][0-9] ]]; then
	echo "✗ ${WAIT}s 内 $URL/healthz 没有给出任何 HTTP 响应（最后一次：${last:-无}）" >&2
else
	echo "✗ ${WAIT}s 后 $URL/healthz 仍不是 200（最后一次：$last）" >&2
fi
exit 1
