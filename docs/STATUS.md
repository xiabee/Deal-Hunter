# Deal-Hunter STATUS

> 给下一次会话用的恢复点。只写有证据的结论：VERIFIED / NOT VERIFIED / BLOCKED / NOT APPLICABLE。
> 长期方向看 [ROADMAP.md](ROADMAP.md)，用法看 [../README.md](../README.md)。

最近更新：2026-09-20（M3 代码侧完成，未部署）

## Current Milestone

**M3 · 本地消费券与开抢前提醒**（设计见 [superpowers/specs/](superpowers/specs/)）。
代码侧完成：`follow_detail` 与列表行日期、`starts_at` 解析、限时事件的日报口径分叉、
事件提醒通道（配置 / 调度 / 三条渲染路径 / 状态字段 / 面板）。

**M3 未完成的部分**：生产配置加南昌信源并部署、区县栏目按准入流程实测、
以及"真实开抢前收到且只收到一次"的实地复核。详见 ROADMAP 的 Current Milestone。

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
| 单元 + 集成测试 | VERIFIED | 202 个测试函数，`go test -race ./...` 本地与 linux-ci 均全绿 |
| 事件提醒端到端（夹具驱动） | VERIFIED | 窗口内恰好一次、跨重启不重发、窗口外（提前 3 天 / 迟到 2 小时）静默、三条渲染路径都带时刻；窗口守卫经"临时翻宽→变红→还原"验证不是空过 |
| 南昌信源可达性 | VERIFIED | 2026-09-20 从生产机出口实测：商务局列表页与详情页 200/UTF-8/SSR，含日期与链接；`/ncszf/tzgg/` 307 跳首页（必须配 `2021_nav_list.shtml`） |
| 真实开抢提醒 | NOT VERIFIED | 需要等下一条带明确时刻且临近开抢的南昌公告；这是 M3 验收的最后一关 |
| 生产部署（M3） | VERIFIED | `alienware-life` 跑 `121e14d`；17 源全部启用；`event={45m/15m/≤2/≥60}`、`due_now=0`、`sent_today=0`；`event_sent=0` 未产生任何骚扰；旧二进制备份为 `deal-hunter.bak-20260920-064343` |
| 南昌源真机抓取 | VERIFIED | 商务局源 `found=1`：那条是 4 月的《如何领取养老服务消费券》问答，**列表行日期解析正确**（probe 打出 2026-04-29），随后被 14 天年龄闸门与 `event.min_score` 双重挡下 —— 即"抓到但拒收"是设计行为而非失效 |
| 短正文守卫真机验证 | VERIFIED | 对局公告外链到 `mp.weixin.qq.com`，取回 65 字，被 `detail body too thin` 正确拒收 |

## M3 的结构性限制（真机确认，别反复尝试）

南昌市商务局消费券公告的正文常**指向微信公众号单篇**，而公众号无法枚举历史、单篇也常只返回
占位页。因此"从列表页解析出开抢时刻"对本市**部分公告成立、对另一部分永远拿不到时刻**。
不追加镜像/爬虫类旁路（会引入不可信的第三方，且违反项目"只读官方页"的立场）；
拿不到时刻的公告就不提醒，是有意为之。区县商务局页面直出正文的可能性更大，按 ROADMAP 的
「信源准入」流程逐条实测后再加。
| 快速门禁（fmt/vet/build/扫描/离线冒烟） | VERIFIED | `bash scripts/ci-local.sh --quick` → `✓ CI PASSED` |
| 全量门禁 | VERIFIED | `DH_CI_HOST=linux-ci bash scripts/ci-office.sh` → `✓ office CI passed (80 files)` |
| 干净环境构建 | VERIFIED | 同上：CI 节点解包到 `/tmp/deal-hunter-ci-*` 全新目录，非本机工作区 |
| 三平台产物 | VERIFIED | 交叉编译 linux/amd64、linux/arm64、windows/amd64 全过；**产物未在目标机运行过** |
| 敏感信息 / 提交身份 | VERIFIED | `secretscan` 无命中；`check-identity.sh` 本地通过（CI 检出无 git 历史 → 直接放行，属预期） |
| 真实工作流（常驻循环） | VERIFIED | linux-ci 上以 `DH_INTERVAL=1m`、`DH_DAILY_AT=+90s` 跑 `run` 300s：排程到点后发出**一份** `items=15` 的日报，当天无第二次，`kind=urgent` 0 次；`state.json` 只见单次 `daily:last_sent`，且 `daily:watched_since` = 进程启动时刻 |
| 部署 | VERIFIED | `alienware-life`（LIFE 域生产机，即基础设施文档里的 life-vm 角色）：install.sh 装 `0cf8416`，旧二进制备份为 `deal-hunter.bak-20260920-034657`，旧配置备份为 `config.json.bak-20260920-034657`，服务 `active` |
| 部署后验收 | VERIFIED | `/healthz` 200；`/api/v1/status` 报 `daily{enabled,at 09:00,sent_today true,next_due 明早 09:00}`、`urgent{≥90,max 1,sent_today 0}`；重启后日志里 `kind=alert|digest` **0 次**；面板 HTML 确实提供新磁贴（今日日报 / 突破推送）；`state.json` 出现 `daily:watched_since` = 重启瞬间 |
| 生产数据完整性 | VERIFIED | 升级后 `deals_seen=514 / pushed=248 / 游标 85`，未丢库；全库链接判定 30 official / 246 third_party / 8 vendor_entry |
| ARM64 实跑 | NOT VERIFIED | 只做了交叉编译，未在 arm64 上 start/smoke |
| GitHub Actions | NOT APPLICABLE | 项目故意不使用（额度），门禁是本地脚本 + 办公室构建机 |

## 次日确认项（2026-09-21 09:00 之后）

1. **是否恰好发了一份日报**：`journalctl -u deal-hunter --since today | grep "kind=daily"` 应为 2 行
   （两个通道各一次），且 `daily:last_sent` 落在 09:00–09:31 之间。今天 09:25 旧版已发过一次，
   新版的 `sent_today` 判定正确地没有补发——这条要在真实跨天后复核。
2. **`live_verified` 是否上升**：目前 0，因为官方链接的联网校验按新设计在**日报发出时**才做。
   若发出后仍长期为 0，说明 `filter.max_official_lookups=16` 对 15 行日报不够（每行最多 3 次外连）。
3. **是否还需要插队**：`urgent.sent_today` 若天天为 1，说明 90 分门槛偏低，读者又被打扰了。

## Current Version

`git describe` 注入；GitHub main 顶端为 `8f3e164`。
生产机跑的二进制戳是 `0cf8416` —— 那是 M2 提交身份改写**之前**的哈希，内容与 `8f3e164`
逐字节相同（`git diff` 为空已验证），只是作者与提交者邮箱从个人邮箱换成了项目的
`@users.noreply.github.com` 地址。所以：**别用 `deal-hunter version` 的哈希去 GitHub 找提交**，
对不上是身份改写的结果，不是部署错了。要一致就用当前顶端重新 `make build-linux` 再装一次。

> 教训（写给下一次会话，也写给自己）：本机 git 的全局默认邮箱不是项目的 noreply 地址，
> 而 `git log` 上一个提交的作者地址也不能证明当前配置就是对的。**提交前**先跑
> `bash scripts/ci-local.sh --quick`（它含 `scripts/check-identity.sh`），
> 并用 `-c user.email=<handle>@users.noreply.github.com` 显式指定身份；
> 构建机上的门禁对无 `.git` 的检出目录会直接放行，帮不了这一步。

## Known Blockers

无 P0/P1 遗留。

## Notes for the next session

- **部署目标是 `alienware-life`**（SSH 里的名字，用户基础设施文档中 LIFE 域的 life-vm 角色）。
  `work-vm` 上从未装过本项目 —— 它是 LIFE 域服务，不要挪过去。
- 面板实际绑定地址来自 `/etc/deal-hunter/deal-hunter.env` 里的 `DH_SERVER_BIND`（当前是一台
  Tailscale mesh 地址），**覆盖** config.json 的 `server.bind`；所以在本机 `curl 127.0.0.1:8765`
  连不上不是故障，要用 env 里那个地址。
- 交付相关的状态键：`daily:last_sent`（RFC3339）、`daily:watched_since`（RFC3339，进程启动即刷新）、
  `urgent:sent`（`{"date":"2006-01-02","n":1}`，按 `timezone` 的本地日记账）。
  `state.json` 里残留着已无人读取的 `digest:last_sent`，没有功能影响，故未动生产状态文件。
- 生产 `/etc/deal-hunter/config.json` 已在本次部署中清理：删掉 digest 与 feishu 的废弃键，
  显式写入 `daily` / `urgent`；`openclaw.{command,args}`（sudo 包装脚本）原样保留。
- 旧版本的 `DH_MIN_SCORE=62` 之类的废弃项已从生产配置移除；env 文件本身未改（密钥未动）。
- `notify.feishu.timezone` 现在只用于卡片页脚时间，与顶层 `timezone` 语义重叠。
- OpenClaw 的开关只有 JSON 里的 `notify.openclaw.enabled`，**没有** `DH_OPENCLAW_ENABLED` 环境变量。
- 临时目录：构建机 `linux-ci` 已清理；`alienware-life:/tmp/dh-deploy-20260920-114649` 留着部署素材，可删。

## Next Candidate

**本地消费券采集 + 开抢前提醒**（用户 2026-09-20 明确提出）。两个前置事实：
只解析 `expires_at`（截止）不够，需要 `starts_at`（开抢）；这一类必须有权走插队通道，不能等第二天的日报。
