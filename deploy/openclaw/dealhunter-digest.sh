#!/usr/bin/env bash
# 羊毛盘点：直接取 Deal-Hunter 的 Markdown 摘要，可原样转发到群里。
# 只读本地 HTTP，不消耗模型 token，不接触任何凭据。
set -euo pipefail
BASE="${DEAL_HUNTER_BASE:-http://127.0.0.1:8765}"
curl -fsS --max-time 10 "$BASE/api/v1/digest" 2>/dev/null || {
  echo "羊毛雷达暂时没连上（$BASE）。可以稍后再问，或检查 deal-hunter 服务状态。"
}
