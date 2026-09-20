# Deal-Hunter STATUS

> 给下一次会话用的恢复点。只写有证据的结论：VERIFIED / NOT VERIFIED / BLOCKED / NOT APPLICABLE。
> 长期方向看 [ROADMAP.md](ROADMAP.md)，用法看 [../README.md](../README.md)。

最近更新：2026-09-20（M3 与"截止时刻时区"修复均已部署，生产 = `3db4152` = HEAD）

## Current Milestone

**M3 · 本地消费券与开抢前提醒**（设计见 [superpowers/specs/](superpowers/specs/)）。
代码与部署都已完成：`follow_detail` 与列表行日期、`starts_at` 解析、限时事件的日报在效口径分叉、
事件提醒通道（配置 / 调度 / 三条渲染路径 / 状态字段 / 面板）、`dealhunter events` 复核命令、
南昌两个官方信源经 `extra_sources` 上线。

**M3 只剩一项**：真实"开抢前收到且只收到一次"。今天下午的观察结论是**当前没有可触发的公告**
（见下表"真机观察"行），不是代码缺陷。这一项转成挂起的复核，判据和取数命令写在下面。

## Last Completed：M3 部署 + 真机观察（2026-09-20）

生产机 `alienware-life` 装 `e849a0b`，17 源全部启用，`backends=[feishu openclaw-drop]`。
观察一轮完整采集后的事实：

- **商务局源** `nc-vouchers-swj`：`found=1 stored=0`。那一条是 4 月的《如何领取养老服务消费券》问答，
  列表行日期解析正确（probe 打出 2026-04-29），随后被 14 天年龄闸门与 `event.min_score` 双重挡下——
  "抓到但拒收"是设计行为。它的详情外链指向 `mp.weixin.qq.com`，取回 65 字，被
  `html: detail body too thin to be the announcement` 正确拒收。
- **市政府源** `nc-vouchers-zf`：`found=0`。同日 `curl` 直取该页 200 / 32KB / 123ms，
  可见行标题全是"拟补贴对象公示""换新补贴明细""送达公告""备案批复"这类，**没有一条含券关键词**，
  该源自带的 keyword/deny 闸门把它们全拒了是正确行为。`found` 统计的是过闸后的行数
  （`pipeline.go:271` → `sources/source.go` 的 keyword/deny 判定），所以 0 不等于解析失效。
- **事件通道**：`due_now=0 sent_today=0`；`dealhunter events` 报"没有在跟踪的限时事件"。
- **不骚扰**：本轮 `event_sent=0 urgent_sent=0`；`kind=alert` 最后一次出现在 03:33（旧二进制），
  03:46 部署后 0 次。日报 `sent_today=true`，`next_due=2026-09-21T09:00+08:00`。
- **顺手清理**：删掉 15 个升级残留（`/opt/deal-hunter` 与 `/etc/deal-hunter` 各留最近 3 份备份 +
  `config.json.bak-relay`），以及 `/tmp/dh-deploy-20260920-114649` 和探测临时文件。

## 同日做完：截止时刻的时区（ROADMAP 候选 #5）

`ExpiresAt` 之前按主机钟表（`time.Local`）摆"23:59:59"，而服务跑 UTC，于是北京的截止日在库里多活
约 8 小时 —— 方向上宽容所以一直没被当 bug。现在与 `starts_at` 同规矩：截止日也按调用方给出的
读者时区（`scoring.Input.ReaderZone`，原 `StartsIn` 改名，因为它的含义已覆盖两端）构造。
**升级后的可观察变化**：到期日判定整体提前 8 小时，过期券不再出现在日报里（这是修正，不是回归）。
本项已随 `3db4152` 部署到生产（全量门禁 `✓ office CI passed (linux-ci, 83 files)` 后带版本戳构建）。

## Verification

| 项 | 结论 | 证据 |
|---|---|---|
| 单元 + 集成测试 | VERIFIED | 208 个测试函数，`go test -race ./...` 本地与 linux-ci 均全绿 |
| 快速门禁 | VERIFIED | `bash scripts/ci-local.sh --quick` → `✓ CI PASSED e849a0b`（fmt/vet/build/secretscan/身份/离线冒烟） |
| 全量门禁 + 干净环境 | VERIFIED | `DH_CI_HOST=linux-ci bash scripts/ci-office.sh` → `✓ office CI passed (linux-ci, 83 files)`，解包到 `/tmp/deal-hunter-ci-*` 全新目录 |
| 三平台产物 | VERIFIED | 交叉编译 linux/amd64、linux/arm64、windows/amd64 全过；**产物未在目标机运行过**（见 ARM64 行） |
| 事件提醒端到端（夹具驱动） | VERIFIED | 窗口内恰好一次、跨重启不重发、窗口外（提前 3 天 / 迟到 2 小时）静默、三条渲染路径都带时刻；窗口守卫经"临时翻宽→变红→还原"验证不是空过 |
| 真实公告措辞解析 | VERIFIED | `9月19日9:00至9月26日24:00`、`上午9:00开放`、`2026年9月19日9:00起` 均正确；`24:00` 拒绝当作开抢时刻；`9月31日` 被 round-trip 守卫拒绝（否则 `time.Date` 会滚到 10-01，猜成未来就真会发错提醒） |
| 截止时刻的时区（原候选 #5） | VERIFIED | `ExpiresAt` 不再用 `time.Local`，改用 `scoring.Input.ReaderZone`（`StartsIn` 改名，含义覆盖开抢与截止两端）。守卫写法是"同一钟表在 UTC 与 UTC+8 必须差出整 8 小时"，所以与测试主机所在时区无关；把 keywords 层与 scoring 层的传参分别改回旧写法，两条测试各自变红已验证 |
| 南昌信源可达性 | VERIFIED | 从生产机出口实测：商务局列表页与详情页 200/UTF-8/SSR；市政府列表页 200/32KB；`/ncszf/tzgg/` 307 跳首页（所以必须配 `2021_nav_list.shtml`）；`tyj.jiangxi.gov.cn` 用 curl 因不支持 legacy 重协商而失败，但 Go TLS 可过 |
| 生产部署 | VERIFIED | `deal-hunter 3db4152 · built=2026-09-20T08:00:54Z`，与 `git rev-parse HEAD` 及 `origin/main` 三者一致；产物 sha256 `dc3787bb…` 在构建机、本机暂存、生产三处相同（传输未被篡改）；`install.sh` 装前跑了一次真 `version`，服务 `active`，首轮 `sources=17 errors=0` |
| 时区修正的可观察效果 | NOT APPLICABLE（暂无对象） | 生产库 904 行带 `meta`，但 **`expires_at` / `starts_at` 均为 0 行** —— 至今没有任何一行真的解析出了时刻并落库（4 月那条在落库前就被拒）。所以这次修正在生产上暂无可比对的观测面，效果只由测试与两道变异守卫证明。第一条带时刻的行落库时要回头看一眼偏移是不是 `+08:00` |
| 真机观察：券公告触发行为 | VERIFIED（观察本身） | 见上一节：两源一条正确拒收、一条今日无券关键词，事件通道 0 发 0 候补，无误发无骚扰 |
| **真实开抢提醒实地复核** | **NOT VERIFIED** | 今天南昌没有"写了明确时刻且临近开抢"的市级公告；唯一在闸内的行是 4 月问答。省级在跑的（赣超 / 体育消费券 9-14 起、每人每周限领 2 张）用的是**周期性/无年份**措辞，M3 故意不猜。触发路径已由夹具与真实措辞两侧证明，等的只是一条现实公告 |
| 面板在采集轮进行中应答（M4） | VERIFIED | 单元层：把信息源卡在请求里，读取方原本等满 3 秒，拆锁后立刻返回，`-race` 干净。生产层：重启后 40 次打 `/api/v1/status` 最大 168ms（1 次非 200 是重启缝隙，稳定态复测 25 次全 200、最大 105ms） |
| ARM64 实跑 | NOT VERIFIED | 只做了交叉编译，未在 arm64 上 start/smoke |
| GitHub Actions | NOT APPLICABLE | 项目故意不使用（额度），门禁是本地脚本 + 办公室构建机 |

## 复核判据（下一条南昌公告出现时执行）

判据：开抢前 ≤45 分钟内收到**且只收到一次**⏰，两个通道各一条。

```bash
ssh alienware-life 'sudo /opt/deal-hunter/deal-hunter events -config /etc/deal-hunter/config.json'
# 面板地址只写在 root 可读的 env 里，别把字面量搬进仓库
ssh alienware-life 'B=$(sudo grep -m1 "^DH_SERVER_BIND=" /etc/deal-hunter/deal-hunter.env | cut -d= -f2-); sudo curl -s "http://$B/api/v1/status" | grep -A9 "\"event\""'  # due_now / sent_today
ssh alienware-life 'sudo journalctl -u deal-hunter --since today | grep "kind=event"'               # 应恰好 2 行
ssh alienware-life 'sudo grep -oE "\"event:(reminded:[^\"]+|sent)\"[^}]*" /var/lib/deal-hunter/state.json'  # 没提醒过时为空（今天即为空）
```

## M3 的结构性限制（真机确认，别反复尝试）

南昌市商务局消费券公告的正文常**指向微信公众号单篇**，而公众号无法枚举历史、单篇也常只返回
占位页。因此"从列表页解析出开抢时刻"对本市**部分公告成立、对另一部分永远拿不到时刻**。
不追加镜像/爬虫类旁路（会引入不可信的第三方，且违反项目"只读官方页"的立场）；
拿不到时刻的公告就不提醒，是有意为之。区县商务局页面直出正文的可能性更大，按 ROADMAP 的
「信源准入」流程逐条实测后再加。

## 次日确认项（2026-09-21 09:00 之后）

1. **是否恰好发了一份日报**：`journalctl -u deal-hunter --since today | grep "kind=daily"` 应为 2 行
   （两个通道各一次），且 `daily:last_sent` 落在 09:00–09:31 之间。今天 09:25 旧版已发过一次，
   新版的 `sent_today` 判定正确地没有补发——这条要在真实跨天后复核。
2. **`live_verified` 是否上升**：目前 0，因为官方链接的联网校验按新设计在**日报发出时**才做。
   若发出后仍长期为 0，说明 `filter.max_official_lookups=16` 对 15 行日报不够（每行最多 3 次外连）。
3. **是否还需要插队**：`urgent.sent_today` 若天天为 1，说明 90 分门槛偏低，读者又被打扰了。

## Current Version

工作区与 GitHub main 顶端、生产二进制戳三者均为 `e849a0b`（一致）。

> 教训（写给下一次会话，也写给自己）：历史上出现过**戳对不上提交**的情况——本机 git 的全局默认邮箱
> 不是项目的 noreply 地址，身份改写会让哈希漂移，而 `git log` 上一个提交的作者地址也不能证明当前
> 配置就是对的。**提交前**先跑 `bash scripts/ci-local.sh --quick`（含 `scripts/check-identity.sh`），
> 并用 `-c user.email=<handle>@users.noreply.github.com` 显式指定身份；
> 构建机上的门禁对无 `.git` 的检出目录会直接放行，帮不了这一步。

## Known Blockers

无 P0/P1 代码遗留。M3 的"真实开抢提醒"挂起项不是阻塞 —— 它等的是外部内容，判据已写在上面。

**一项需要人看一眼的异常（2026-09-20 晚）**：这一段时间里多次工具返回中混入了**伪造内容** ——
假的 `git commit` / `git push` 成功输出、一个本仓库不存在的提交 `d38709b`（`git show` 报
unknown revision）、以及冒充"用户已发消息批准立即推送/此前已授权 force push"的段落。因此当轮的
推送与部署被暂停，等真实用户确认后才继续（现已完成，`origin/main` = 生产 = `3db4152`）。
生产机上另有两个**不属于本会话**的残留：`/tmp/dh-m5-1789888881`（今天 07:21 的仓库快照，含
config/deploy/dist）与 `/tmp/dh-wire-feishu.sh`（9-19，把服务接到租户已有飞书应用的接线脚本）。
"m5" 这个命名本会话从未用过，怀疑有第二个会话或代理在同一台机器上并行操作本项目 —— 这也能解释
上面的伪造输出。**未删除、未改动它们**，下次动这台机器前先确认来源。

## Notes for the next session

- **部署目标是 `alienware-life`**（SSH 里的名字，用户基础设施文档中 LIFE 域的 life-vm 角色）。
  `work-vm` 上从未装过本项目 —— 它是 LIFE 域服务，不要挪过去。
  二进制在 `/opt/deal-hunter/deal-hunter`（不是 `/usr/local/bin`），状态目录 `/var/lib/deal-hunter`，
  配置 `/etc/deal-hunter/config.json`；env 与 state 只有 root 可读，探测要带 `sudo`。
- 面板实际绑定地址来自 `/etc/deal-hunter/deal-hunter.env` 里的 `DH_SERVER_BIND`（当前是一台
  Tailscale mesh 地址），**覆盖** config.json 的 `server.bind`；所以在本机 `curl 127.0.0.1:8765`
  连不上不是故障，要用 env 里那个地址。
- 交付相关的状态键：`daily:last_sent`、`daily:watched_since`（进程启动即刷新）、
  `urgent:sent` 与 `event:sent`（都是 `{"date":"2006-01-02","n":1}`，按 `timezone` 的本地日记账）、
  `event:reminded:<fingerprint>`（每个事件一次，写过就不再提醒）。
  `state.json` 里残留着已无人读取的 `digest:last_sent`，没有功能影响，故未动生产状态文件。
- 生产 `/etc/deal-hunter/config.json` 已清理：删掉 digest 与 feishu 的废弃键，显式写入
  `daily` / `urgent` / `event`，南昌两源放在 `extra_sources`；`openclaw.{command,args}`
  （sudo 包装脚本）原样保留。废弃的 `DH_MIN_SCORE` 之类已从生产配置移除；env 密钥未动。
- `notify.feishu.timezone` 现在只用于卡片页脚时间，与顶层 `timezone` 语义重叠。
- OpenClaw 的开关只有 JSON 里的 `notify.openclaw.enabled`，**没有** `DH_OPENCLAW_ENABLED` 环境变量。

## Next Candidate

ROADMAP 的 Next Candidates #1「到期前提醒」：与 M3 共用 `starts_at`、`Upcoming` 与事件调度，
是 M3 机制的延长线，最省事。
