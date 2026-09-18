# Deal-Hunter · 羊毛情报技能（OpenClaw 侧）

让 OpenClaw 助手（已连接飞书）随时读取本机 **Deal-Hunter** 采集到的折扣与免费额度情报。

**强约束：**只读本机 HTTP 接口，不调用模型、不读写 OpenClaw 凭据、不修改 Deal-Hunter 配置。
主动播报由 Deal-Hunter 自己通过飞书群机器人完成，本技能只负责「被问到时答得准」。

## 前置条件

- 同机运行 Deal-Hunter，只读 API 监听 `127.0.0.1:8765`（默认；跨机器请用私有 mesh 地址）。
- 无需任何 token：接口只读、不含密钥。

## 可用查询脚本

| 脚本 | 作用 |
|---|---|
| `dealhunter-digest.sh` | 最近一轮盘点（Markdown，适合直接转发到群） |
| `dealhunter-deals.sh [min] [limit]` | 高分羊毛清单，默认 min=60 limit=10 |
| `dealhunter-status.sh` | 服务健康度：采集轮次、入库量、推送通道 |

## 底层接口（全部 GET，只读）

| 接口 | 说明 |
|---|---|
| `/api/v1/digest` | Markdown 摘要，可直接朗读/转发 |
| `/api/v1/deals?min=&limit=&category=&q=` | 结构化结果：`{count, deals:[{title,url,score,source,vendors,is_free,...}]}` |
| `/api/v1/status` | 运行状态、阈值、信息源统计、待推送数量 |
| `/api/v1/sources` | 各信息源最近一轮命中/耗时/错误 |
| `/healthz` | 存活探针 |

## 用法示例

```bash
bash ~/.openclaw/workspace/deal-hunter/dealhunter-digest.sh
bash ~/.openclaw/workspace/deal-hunter/dealhunter-deals.sh 70 5
curl -s http://127.0.0.1:8765/api/v1/status | head -40
```

## 回答建议话术

> 「刚看到 {count} 条新羊毛，最值得领的是：{title}（置信分 {score}），入口 {url}，{summary}」

- 只报**有链接、有分数**的条目；分数低于 60 的默认不提。
- 若接口不可达，直接说「羊毛雷达暂时没连上」，不要编造优惠信息。
- 折扣有实时性：提醒用户以厂商页面为准。

## 安全边界

- 该 API 只绑定回环或私有 mesh 地址，配置层会拒绝公网绑定。
- Deal-Hunter 的飞书 webhook 与签名密钥只存在于服务端 `0600` 环境文件，
  既不出现在接口响应里，也不写入仓库（CI 有敏感信息扫描门禁）。
