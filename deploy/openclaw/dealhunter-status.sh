#!/usr/bin/env bash
# Deal-Hunter 健康度：轮次、入库、待推送、通道就绪情况。
# 只读本地 HTTP，不接触任何凭据。
set -euo pipefail
BASE="${DEAL_HUNTER_BASE:-http://127.0.0.1:8765}"
curl -fsS --max-time 10 "$BASE/api/v1/status" 2>/dev/null \
  || echo "{\"error\":\"deal-hunter unreachable\",\"base\":\"$BASE\"}"
