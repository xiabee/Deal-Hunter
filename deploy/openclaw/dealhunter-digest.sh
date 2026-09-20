#!/usr/bin/env bash
# 最近一份消息：取 Deal-Hunter 落盘的 Markdown 摘要（日报或插队），可原样转发到群里。
# 只读本地 HTTP，不消耗模型 token，不接触任何凭据。
set -euo pipefail
BASE="${DEAL_HUNTER_BASE:-http://127.0.0.1:8765}"
curl -fsS --max-time 10 "$BASE/api/v1/digest" 2>/dev/null || {
  echo "羊毛雷达暂时没连上（$BASE）。可以稍后再问，或检查 deal-hunter 服务状态。"
}
