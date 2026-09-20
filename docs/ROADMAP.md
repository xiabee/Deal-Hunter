# Deal-Hunter Roadmap

> 这份文件是项目的长期记忆，取代 README 里的勾选清单。
> 每个 Milestone 都必须能实际验收；做完立刻回来更新，不要让它变成愿望清单。

## Vision

跑在自家服务器上的羊毛侦察兵：宽采集、狠去重、可解释评分，**一天一份日报**。
单二进制、零第三方依赖、无数据库、默认不对外网暴露。

## Current State

- 交付形态已从「发现即推的雷达」改为「每天一份日报 + 高分当天插队一条」（M2，本次提交）。
- 采集、评分、官方链接校准、近似去重、四条投递路径、只读面板均在位且有离线测试。
- 门禁是本机与办公室构建机上的脚本；**不使用 GitHub Actions**。
- 部署形态仍是一个 systemd 单元 + `/var/lib/deal-hunter` 状态目录（`deploy/install.sh`）。

## Completed Milestones

| # | 内容 | 落点 |
|---|---|---|
| M0 | 全网羊毛雷达初版：自主采集 + 置信评分 + 飞书/OpenClaw 双投递 | `723a480` |
| M1 | 官方链接回查与校验（三级判定 + 预算 + 7 天缓存）、近似标题折叠、只读面板 | `9998ef4`…`5ed7310` |
| M2 | **一天一份日报**：09:00 日报为唯一排程消息，高分当天最多插队一条；空库也发；修复新装上日报永不触发的死锁 | 本次提交 |

## Current Milestone

**M3 · 本地消费券与开抢前提醒** —— 设计见
[superpowers/specs/2026-09-20-voucher-event-reminders-design.md](superpowers/specs/2026-09-20-voucher-event-reminders-design.md)。
代码侧已完成：采集器 `follow_detail` 与列表行日期、`starts_at` 解析、日报在效判定按类别分叉、
事件提醒通道（配置 / 调度 / 三条渲染路径 / 面板与状态字段）。

**还差三件事才算完成**：

1. ~~生产配置加上验证过的南昌信源并部署~~ 已完成：两源经 `extra_sources` 上线，17/17 启用。
2. 区县栏目按「信源准入」流程逐条实测后再加（可达 + 编码 + 列表结构 + 含券样本）。
3. 真实复核：等下一条带明确开抢时刻的公告，确认"开抢前收到且只收到一次"。

## Completed Milestones

| # | 内容 | 落点 |
|---|---|---|
| M0 | 全网羊毛雷达初版：自主采集 + 置信评分 + 飞书/OpenClaw 双投递 | `723a480` |
| M1 | 官方链接回查与校验（三级判定 + 预算 + 7 天缓存）、近似标题折叠、只读面板 | `9998ef4`…`5ed7310` |
| M2 | **一天一份日报**：09:00 日报为唯一排程消息，高分当天最多插队一条；空库也发；修复新装上日报永不触发的死锁 | `80721b3`…`8f3e164`，已部署 `alienware-life` |
| M3 部分 | 采集 `follow_detail` / 列表行日期；`starts_at` 解析；限时事件不再享受"无期限即在效"；事件提醒通道 | 本次提交 |
| M4 | 采集轮进行中不再锁住只读面板：roundMu 串行化轮次，mu 只保护运行历史 | `396543f` |

## Next Candidates（按价值排序，未开工）

1. **到期前提醒**：日报已展示截止日，缺「明天到期」单独一条 —— 与 M3 共用 `starts_at` 与事件调度，
   M3 收尾后接着做最省事。
2. **更多官方目录差分**：推理云平台 / 向量库 / GPU 云，复用 `openrouter` 的游标差分套路。
3. **ARM64 与真实部署验证**：交叉编译已过，但从未在 linux/arm64 与 systemd 下实跑过（见 Known Risks）。
4. **把身份门禁扩到文件内容**（小改动，价值高）：`check-identity.sh` 只查提交作者，`internal/secretlint`
   只查凭证特征与内网拓扑，所以**文档里写死一个个人邮箱地址不会触发任何一道闸**。M2 部署后写 STATUS
   时正好差点把刚抹掉的地址重新公开出去。修法：给 secretlint 增加一类消费者邮箱特征
   （foxmail / qq / 163 / gmail…），并配一套豁免（SECURITY.md 的披露联系地址必须能留下来）。
   验收：一个 fixture 用例证明命中即失败、一个用例证明白名单生效。
5. **`ExpiresAt` 的时区**：它用 `time.Local` 构造时刻而比较用绝对时间，服务跑 UTC 时北京的截止日会
   多活约 8 小时。方向上宽容（只晚报不遗漏），所以单独排期，见 spec §13。

## Known Risks

- **投递扇出任一成功即算送达**：一天只有一份消息，所以某个通道坏了当天这份就只存在于日志里。
  目前靠 `notify: partially delivered` 警告与 `/api/v1/status` 暴露，不做重试。真需要更强保证时要先定策略（去重 vs 漏投）。
- **日报只在采集轮结束后检查**：`daily.at` 到点后最多再等一个 `interval`（默认 30m）才发；发送失败下一轮重试。
- **插队门槛 90 分是估值**：「模型刚刚转免费」这类事件实测是否稳定 ≥90 未在真实采集里验证过（评分是加法的，理论 87–97）。
- **`state.json` 里会残留 `digest:last_sent`**：盘点通道已删，旧键没人读也没人清理；`Compact` 只重写 `deals.jsonl`。
- **升级后旧配置里的废弃键不报错也不生效**（`notify.digest`、`notify.feishu.{min_score,max_per_run,silent_hours}`、`filter.min_score`），`install.sh` 会打一行提示。

## Technical Debt

- `internal/scheduler` 没有测试文件：`Loop` 直接依赖 `*pipeline.App`，要测触发逻辑得先抽一个小接口（**故意没做**，避免为测试造抽象）。
- `notify.feishu.timezone` 与顶层 `timezone` 语义重叠，前者现在只用于卡片页脚时间。
- `notify.DealsOf` 只为测试存在。
- 采集器类型有 7 种，`sources.New` 是一个 switch；再加两类以上时值得回到注册表写法。

## Non-Goals

- 不引入数据库、Docker、消息队列、微服务拆分或前端构建链（JSONL + 单二进制是特性不是债）。
- 不用 GitHub Actions 当门禁（额度优先留给代码托管与 Release）。
- 不做多群路由 / 分类订阅：目前只有一个读者。
- 不把 `0.0.0.0` 暴露成公网服务，也不为公网访问写认证层。
