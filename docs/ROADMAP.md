# Deal-Hunter Roadmap

> 这份文件是项目的长期记忆，取代 README 里的勾选清单。
> 每个 Milestone 都必须能实际验收；做完立刻回来更新，不要让它变成愿望清单。

## Vision

跑在自家服务器上的羊毛侦察兵：宽采集、狠去重、可解释评分，**一天一份日报**。
单二进制、零第三方依赖、无数据库、默认不对外网暴露。

## Current State

- 交付形态已从「发现即推的雷达」改为三类消息：每天一份日报、高分当天最多插队一条、限时事件开抢前提醒（M2/M3）。
- 采集、评分、官方链接校准、近似去重、投递后端（飞书卡片 + OpenClaw drop/relay + console）、只读面板均在位且有离线测试。
- 门禁是本机与办公室构建机上的脚本；**不使用 GitHub Actions**。
- 部署形态仍是一个 systemd 单元 + `/var/lib/deal-hunter` 状态目录（`deploy/install.sh`）。

## Current Milestone

**M3 · 本地消费券与开抢前提醒** —— 设计见
[superpowers/specs/2026-09-20-voucher-event-reminders-design.md](superpowers/specs/2026-09-20-voucher-event-reminders-design.md)。
代码、部署与真机观察都已完成：采集器 `follow_detail` 与列表行日期、`starts_at` 解析、
限时事件的日报在效口径分叉、事件提醒通道（配置 / 调度 / 三条渲染路径 / 面板与状态字段）、
`dealhunter events` 复核命令、南昌两源经 `extra_sources` 上线（17/17 启用）。

**只剩两件**：

1. 真实复核"到时候收到且只收到一次"（开抢与到期共用同一条通道、同一个每行标记）。
   **今天无法执行**：南昌当下没有写了明确时刻且临近开抢或截止的市级公告，唯一在闸内的行是 4 月的
   问答（正确拒收），在跑的省级活动用的是周期性 / 无年份措辞（M3 故意不猜）。而且生产库里至今
   **0 行带 `expires_at` / `starts_at`** —— 触发路径已由夹具与真实措辞两侧证明，等的只是一条现实公告；
   判据与取数命令在 STATUS 的「复核判据」。
2. 区县栏目按「信源准入」流程逐条实测后再加（可达 + 编码 + 列表结构 + 含券样本）。


## Completed Milestones

| # | 内容 | 落点 |
|---|---|---|
| M0 | 全网羊毛雷达初版：自主采集 + 置信评分 + 飞书/OpenClaw 双投递 | `723a480` |
| M1 | 官方链接回查与校验（三级判定 + 预算 + 7 天缓存）、近似标题折叠、只读面板 | `9998ef4`…`5ed7310` |
| M2 | **一天一份日报**：09:00 日报为唯一排程消息，高分当天最多插队一条；空库也发；修复新装上日报永不触发的死锁 | `80721b3`…`8f3e164`，已部署 `alienware-life` |
| M3（代码与部署，实地复核挂起） | 采集 `follow_detail` / 列表行日期；`starts_at` 解析；限时事件不再享受"无期限即在效"；事件提醒通道；`dealhunter events`；南昌两源上线 | `121e14d`…`e849a0b`，已部署 `alienware-life` |
| M4 | 采集轮进行中不再锁住只读面板：roundMu 串行化轮次，mu 只保护运行历史 | `396543f` |
| M5 | 到期前提醒复用同一张 ⏰ 卡（`expires_at` 侧）：`Store.Expiring`、`notify.event.expiry_lead`、两个 due 计数、只提醒未过期的截止；面板日间/夜间配色切换 | 本次提交 |
| M6（门禁加固） | `secretlint` 新增 `consumer_mailbox`：个人邮箱写进**文档正文**也会被拦（`check-identity` 只管提交作者），豁免沿用同行 `secretlint:ignore` + 理由 | 本次提交 |

## Next Candidates（按价值排序，未开工）

1. **更多官方目录差分**：推理云平台 / 向量库 / GPU 云，复用 `openrouter` 的游标差分套路。
2. **ARM64 与真实部署验证**：交叉编译已过，但从未在 linux/arm64 与 systemd 下实跑过（见 Known Risks）。

## Known Risks

- **投递扇出任一成功即算送达**：一天只有一份消息，所以某个通道坏了当天这份就只存在于日志里。
  目前靠 `notify: partially delivered` 警告与 `/api/v1/status` 暴露，不做重试。真需要更强保证时要先定策略（去重 vs 漏投）。
- **日报只在采集轮结束后检查**：`daily.at` 到点后最多再等一个 `interval`（默认 30m）才发；发送失败下一轮重试。
- **插队门槛 90 分是估值**：「模型刚刚转免费」这类事件实测是否稳定 ≥90 未在真实采集里验证过（评分是加法的，理论 87–97）。
- **`state.json` 里会残留 `digest:last_sent`**：盘点通道已删，旧键没人读也没人清理；`Compact` 只重写 `deals.jsonl`。
- **升级后旧配置里的废弃键不报错也不生效**（`notify.digest`、`notify.feishu.{min_score,max_per_run,silent_hours}`、`filter.min_score`），`install.sh` 会打一行提示。

## Technical Debt

- `deploy/install.sh` 只接受带路径的二进制：`sudo ./deploy/install.sh dealhunter-linux-amd64` 会把它当
  命令去找并报"无法在本机执行"。修法是一行（无斜杠时前缀 `./`）。本次部署踩过一次，记在这里备修。
- `internal/scheduler` 没有测试文件：`Loop` 直接依赖 `*pipeline.App`，要测触发逻辑得先抽一个小接口（**故意没做**，避免为测试造抽象）。
- `notify.feishu.timezone` 与顶层 `timezone` 语义重叠，前者现在只用于卡片页脚时间。
- `notify.DealsOf` 只为测试存在。
- 采集器类型有 7 种，`sources.New` 是一个 switch；再加两类以上时值得回到注册表写法。

## Non-Goals

- 不引入数据库、Docker、消息队列、微服务拆分或前端构建链（JSONL + 单二进制是特性不是债）。
- 不用 GitHub Actions 当门禁（额度优先留给代码托管与 Release）。
- 不做多群路由 / 分类订阅：目前只有一个读者。
- 不把 `0.0.0.0` 暴露成公网服务，也不为公网访问写认证层。
