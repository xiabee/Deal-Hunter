#!/usr/bin/env bash
# 高分羊毛清单。用法：dealhunter-deals.sh [最低分数=60] [条数=10] [关键词]
# 只读本地 HTTP，不消耗模型 token，不接触任何凭据。
set -euo pipefail
BASE="${DEAL_HUNTER_BASE:-http://127.0.0.1:8765}"
MIN="${1:-60}"
LIMIT="${2:-10}"
QUERY="${3:-}"

url="$BASE/api/v1/deals?min=$MIN&limit=$LIMIT"
[[ -n "$QUERY" ]] && url="$url&q=$(printf '%s' "$QUERY" | sed 's/ /%20/g')"

curl -fsS --max-time 10 "$url" 2>/dev/null || {
  echo "{\"error\":\"deal-hunter unreachable\",\"base\":\"$BASE\"}"
  exit 0
}
