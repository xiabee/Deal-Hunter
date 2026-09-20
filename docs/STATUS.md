# Deal-Hunter STATUS

> 给下一次会话用的恢复点。只写有证据的结论：VERIFIED / NOT VERIFIED / BLOCKED / NOT APPLICABLE。
> 长期方向看 [ROADMAP.md](ROADMAP.md)，用法看 [../README.md](../README.md)。

最近更新：2026-09-20（M2 完成时）

## Current Milestone

无 —— M2 已完成并提交，等待选定下一项（首选：本地消费券，见 ROADMAP Next Candidates #1）。

## Last Completed：M2 交付形态改为「一天一份日报」

- 每天 `notify.daily.at`（默认 09:00，按 `timezone` 解释）发**一份**日报；当天没有在效的也发空报。
- 分数 ≥ `notify.urgent.min_score`（默认 90）的发现当天可插队**一条**，用 `urgent:sent` 按本地日记账。
- 退役并删除：`notify.digest` 整条通道、`notify.feishu.{min_score,max_per_run,silent_hours,at_all,dedupe_minutes}`、
  `dealhunter digest` 命令、`Store.Pending/Pushed`、以及从未生效的 `filter.min_score` / `DH_MIN_SCORE`。
- 顺带修掉的 bug：
  - **新装上日报永远不触发**（`daily:last_sent` 只由 `SendDaily` 写、而只有 `DailyDue` 为真才走到 `SendDaily`；
    没有任何地方播种游标 → 死锁）。现在进程启动即写 `daily:watched_since` 作为闸门，
    既保留「不补发启动前已过去的时段」，又保证下一个时段一定触发。
  - **面板永远显示「飞书 未配置」**：前端读 `s.feishu_ready`，接口却把它嵌在 `notify` 下。
  - 日报入选条目在发出前才做官方链接校验（`verifyForBriefing`），否则新模型下 ✅ 徽章几乎不会再出现。
  - SECURITY.md 引用的 `TestFanOutToleratesOneFailingBackend` 早就不存在，已改为真实测试名。

## Verification

| 项 | 结论 | 证据 |
|---|---|---|
| 单元 + 集成测试 | VERIFIED | 185 个测试函数，`go test -race ./...` 本地与 linux-ci 均全绿 |
| 快速门禁（fmt/vet/build/扫描/离线冒烟） | VERIFIED | `bash scripts/ci-local.sh --quick` → `✓ CI PASSED` |
| 全量门禁 | VERIFIED | `DH_CI_HOST=linux-ci bash scripts/ci-office.sh` → `✓ office CI passed (80 files)` |
| 干净环境构建 | VERIFIED | 同上：CI 节点解包到 `/tmp/deal-hunter-ci-*` 全新目录，非本机工作区 |
| 三平台产物 | VERIFIED | 交叉编译 linux/amd64、linux/arm64、windows/amd64 全过；**产物未在目标机运行过** |
| 敏感信息 / 提交身份 | VERIFIED | `secretscan` 无命中；`check-identity.sh` 本地通过（CI 检出无 git 历史 → 直接放行，属预期） |
| 真实工作流（常驻循环） | VERIFIED | linux-ci 上以 `DH_INTERVAL=1m`、`DH_DAILY_AT=+90s` 跑 `run` 300s：排程到点后发出**一份** `items=15` 的日报，当天无第二次，`kind=urgent` 0 次；`state.json` 只见单次 `daily:last_sent`，且 `daily:watched_since` = 进程启动时刻 |
| 部署 | NOT VERIFIED | 本次未部署；改动只到构建 |
| 部署目标 | BLOCKED | 生活域默认 `life-vm`，但当前 `~/.ssh/config` 里没有 `life-vm` 条目（有 `alienware-life`/`work-vm`/`linux-ci`/`kylin-pc`/`ALIENWARE`）；需确认这台机器上是否已有 deal-hunter 服务，再决定是首发还是升级 |
| ARM64 实跑 | NOT VERIFIED | 只做了交叉编译，未在 arm64 上 start/smoke |
| GitHub Actions | NOT APPLICABLE | 项目故意不使用（额度），门禁是本地脚本 + 办公室构建机 |

## Current Version

`git describe` 注入（无 tag 时为 `cbed755-dirty` 一类）；`dealhunter version` 打印 version/commit/buildDate。
未发布 GitHub Release。

## Known Blockers

无 P0/P1 遗留。

## Notes for the next session

- 交付相关的状态键：`daily:last_sent`（RFC3339）、`daily:watched_since`（RFC3339，进程启动即刷新）、
  `urgent:sent`（`{"date":"2006-01-02","n":1}`，按 `timezone` 的本地日记账）。
  `state.json` 里可能残留已无人读取的 `digest:last_sent`。
- 部署过旧版本的机器，`/etc/deal-hunter/config.json` 与 `deal-hunter.env` 里的废弃键不报错也不生效；
  `install.sh` 升级分支会打印一行提示。旧的 `DH_MIN_SCORE=62` 从此彻底无意义（它本来也没生效过）。
- `notify.feishu.timezone` 现在只用于卡片页脚时间，与顶层 `timezone` 语义重叠。
- OpenClaw 的开关只有 JSON 里的 `notify.openclaw.enabled`，**没有** `DH_OPENCLAW_ENABLED` 环境变量。
- 远端跑过一次冒烟的构建机上留有 `/tmp/dh-smoke` 与一个 `/tmp/deal-hunter-ci-*` 工作区，可随意删除。

## Next Candidate

**本地消费券采集 + 开抢前提醒**（用户 2026-09-20 明确提出）。两个前置事实：
只解析 `expires_at`（截止）不够，需要 `starts_at`（开抢）；这一类必须有权走插队通道，不能等第二天的日报。
