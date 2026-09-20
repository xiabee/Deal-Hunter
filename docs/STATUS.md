# Deal-Hunter STATUS

> 给下一次会话用的恢复点。只写有证据的结论：VERIFIED / NOT VERIFIED / BLOCKED / NOT APPLICABLE。
> 长期方向看 [ROADMAP.md](ROADMAP.md)，用法看 [../README.md](../README.md)。

最近更新：2026-09-20（M5 到期前提醒 + 面板日间模式做完；生产仍为 `3db4152`，待部署这一版）

## Current Milestone

**M5 · 到期前提醒（复用 M3 的事件通道）** —— 用户批准的形态：不新增第四类消息，同一张 ⏰ 卡
既报"要开抢"也报"要作废"，两个钟共用 `event:reminded:<fp>` 与 `event:sent` 预算。
落点：`Store.Expiring`、`notify.event.expiry_lead`（默认 3h，`0s` 只关到期侧）、
`status.event` 拆出 `due_opening` / `due_expiry`、`dealhunter events` 增"事由"列、
doctor 报"到期前 3h0m0s"、面板 hint 分开显示两个数。顺带：面板加日间模式（默认跟随系统）。

**M3 的挂起项没变，只是范围扩到两端**：真实"到时候收到且只收到一次"仍 NOT VERIFIED ——
生产里此刻只有 1 行带时刻（一条 linux.do 的免费额度，开抢窗口已过），南昌仍没有临近开抢或
截止的公告。判据见下面的「复核判据」。

### 上一节：M3 · 本地消费券与开抢前提醒（设计见 [superpowers/specs/](superpowers/specs/)）

代码与部署都已完成：`follow_detail` 与列表行日期、`starts_at` 解析、限时事件的日报在效口径分叉、
事件提醒通道（配置 / 调度 / 三条渲染路径 / 状态字段 / 面板）、`dealhunter events` 复核命令、
南昌两个官方信源经 `extra_sources` 上线。它的挂起项已由 M5 接上同一张卡。

## Last Completed：M5 到期前提醒 + 面板日间模式（2026-09-20）

**到期侧**：`Store.Expiring(from, through, min)`（与 `Upcoming` 同形状，键换 `expires_at`）；
`dueEvents` 合并两个锚点、按"还有多久发生"排序、**同一行取更近的那个钟**，仍受
`event:reminded:<fp>` 一次性约束，预算仍是 `event:sent`。到期只提醒**还没过去**的截止时刻
（`from = now`），过期即静默。`expiry_lead=0s` 只关到期侧、不动开抢侧。

四条守卫都做了变异复查（改回旧写法必红）：到期窗口内提醒一次、`max_items` 截断且第三行不留
"已提醒"标记、标题带上"截止"字样、只有截止时刻的行也进 `events` 清单与面板 due 计数。
`dealhunter events` 之前会把只有截止的行显示成"开抢时刻 01-01 08:05 / -2562048小时前"，
现在按 `Moment` + `事由` 显示 —— 这就是加那条 CLI 测试的理由。

**日间模式**：`index.html` 的配色全部走 `:root` 变量，新增 `[data-theme=light]` 一组覆盖；
原先 7 处 `rgba(255,255,255,.0x)` 覆盖层与 4 处浅紫/浅青文字收进 `--glass/--glass2/--edge/
--sum/--tint-*`，否则在纸白底上直接看不见。切换按钮在顶栏，首屏顺序 `localStorage('dh-theme')`
→ `prefers-color-scheme` → 暗色，判定写在 `<style>` 之前以免刷新闪白。
门禁能测的是"浅色块重定义了 5 个核心变量 + 正文对底色对比度 ≥ WCAG AA 4.5:1 + 不留硬编码白覆盖层"；
**好不好看没测也没有工具能测，需要人眼过一遍**。

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
| 三平台产物 | VERIFIED | 交叉编译 linux/amd64、linux/arm64、windows/amd64 全过；arm64 产物已在真机跑过（见下一行），amd64 产物跑在生产机 |
| 事件提醒端到端（夹具驱动） | VERIFIED | 窗口内恰好一次、跨重启不重发、窗口外（提前 3 天 / 迟到 2 小时）静默、三条渲染路径都带时刻；窗口守卫经"临时翻宽→变红→还原"验证不是空过 |
| 真实公告措辞解析 | VERIFIED | `9月19日9:00至9月26日24:00`、`上午9:00开放`、`2026年9月19日9:00起` 均正确；`24:00` 拒绝当作开抢时刻；`9月31日` 被 round-trip 守卫拒绝（否则 `time.Date` 会滚到 10-01，猜成未来就真会发错提醒） |
| 截止时刻的时区（原候选 #5） | VERIFIED | `ExpiresAt` 不再用 `time.Local`，改用 `scoring.Input.ReaderZone`（`StartsIn` 改名，含义覆盖开抢与截止两端）。守卫写法是"同一钟表在 UTC 与 UTC+8 必须差出整 8 小时"，所以与测试主机所在时区无关；把 keywords 层与 scoring 层的传参分别改回旧写法，两条测试各自变红已验证 |
| 南昌信源可达性 | VERIFIED | 从生产机出口实测：商务局列表页与详情页 200/UTF-8/SSR；市政府列表页 200/32KB；`/ncszf/tzgg/` 307 跳首页（所以必须配 `2021_nav_list.shtml`）；`tyj.jiangxi.gov.cn` 用 curl 因不支持 legacy 重协商而失败，但 Go TLS 可过 |
| 生产部署 | VERIFIED | `deal-hunter e23a59f · built=2026-09-20T17:16:15Z`（= `git log -1 -- ':!docs'`）；今晚四次部署（`378fd8d`→`9daa3d3`→`1209fe0`→`e23a59f`）产物 sha256 在构建机 / 本机暂存 / 生产三处逐次一致；`healthz` 200，17 源，首轮 0 错误，备份各留最近 3 份 |
| 只读命令不再换掉状态文件所有者 | VERIFIED（生产实测） | 修复前后各跑一次 `sudo dealhunter events` + `doctor`：`state.json` 的所有者与 mtime 都不再变动（修复前它会变成 `root:root`，服务下次写状态即 permission denied 进入崩溃循环 —— 今晚真实发生并恢复过一次） |
| 个人邮箱不再能溜进文件内容（M6） | VERIFIED | `internal/secretlint` 加 `consumer_mailbox`（`check-identity` 只看提交作者，正文里写死地址此前过得了所有闸）。两条夹具：命中即报（文档行与代码行各一）、同行 `secretlint:ignore` + 理由可豁免。变异复查过：把规则改名即"reported 0 findings"变红。**顺带证明门禁自身有效**：加规则的当场，`TestRepositoryIsOpenSourceClean` 就把我的假邮箱夹具拦下了，按既有惯例用带理由的标记放行 |
| 时区修正的可观察效果 | NOT APPLICABLE（暂无对象） | 生产库 904 行带 `meta`，但 **`expires_at` / `starts_at` 均为 0 行** —— 至今没有任何一行真的解析出了时刻并落库（4 月那条在落库前就被拒）。所以这次修正在生产上暂无可比对的观测面，效果只由测试与两道变异守卫证明。第一条带时刻的行落库时要回头看一眼偏移是不是 `+08:00` |
| 真机观察：券公告触发行为 | VERIFIED（观察本身） | 见上一节：两源一条正确拒收、一条今日无券关键词，事件通道 0 发 0 候补，无误发无骚扰 |
| **真实"到时候提醒"实地复核（开抢与到期两端）** | **NOT VERIFIED** | 今天南昌没有"写了明确时刻且临近开抢或截止"的市级公告；唯一在闸内的行是 4 月问答。省级在跑的（赣超 / 体育消费券 9-14 起、每人每周限领 2 张）用的是**周期性/无年份**措辞，M3 故意不猜。触发路径已由夹具与真实措辞两侧证明，等的只是一条现实公告 |
| 到期前提醒（M5，夹具驱动） | VERIFIED | 窗口内（2h / lead 3h）恰好一次且跨轮不重发；还有 5 天与已过期 30 分钟都静默；`max_items=2` 截断后第三行不留标记；标题带"截止"；只有截止时刻的行进 `events` 清单与 `due_expiry` 计数。四条守卫逐个变异复查过（把实现改回旧写法即红） |
| 起止区间写法的截止（生产真丢过一条） | VERIFIED | 部署 `378fd8d` 后 `dealhunter events` 打出生产第一条带时刻的行：`09-18 10:00 开抢 … 89 分 Qwen3.8-Flash`，而 `expires_at` 为空 —— 原文是「新加坡时间：2026年9月18日10:00至2026年9月30日23:59」，区间没有"截止"前置词，于是按 M3 口径整条掉出日报。加 `rangeEnd`（两个小正则：区间前后各须一个完整日期）后修复。端到端守卫 `TestBriefingKeepsADatedEventWrittenAsARange` 与单元守卫都做过变异复查：短路 `rangeEnd` 即"got 0 rows" |
| 面板日间模式（M5） | VERIFIED（结构） / NOT VERIFIED（观感） | 结构守卫：浅色块必须重定义 `--bg/--panel/--txt/--dim/--line`，暗与亮两套 `--txt` 对 `--bg` 的对比度都 ≥4.5:1（测试里现算 WCAG），且不允许残留 `rgba(255,255,255` 硬编码。配色好不好看**没有工具能证明**，需读者在浏览器里看一眼 |
| 面板在采集轮进行中应答（M4） | VERIFIED | 单元层：把信息源卡在请求里，读取方原本等满 3 秒，拆锁后立刻返回，`-race` 干净。生产层：重启后 40 次打 `/api/v1/status` 最大 168ms（1 次非 200 是重启缝隙，稳定态复测 25 次全 200、最大 105ms） |
| ARM64 实跑 | VERIFIED | 2026-09-21 在 `kylin-pc`（Kylin V10 SP1，`Linux aarch64`）跑 `25afd4c` 交叉编译产物：`file` 确认 ELF aarch64 静态；sha256 `ede936fd…` 两端一致；`version` 报 `go1.26.4/linux-arm64`；`doctor -net=false` 体检通过；`run` 真实起了一轮（`sources=15 new=67 stored=67 errors=0 took=4.4s`）；面板在 127.0.0.1 上 `healthz=200`、`/` 里 `data-theme` 命中 6 次。**注意**：这是临时冒烟测试，没有装 systemd 单元，kylin-pc 也不是本项目的部署目标（部署仍在 `alienware-life`） |
| 校验和与传输完整性 | VERIFIED | 今晚五次产物传输（amd64 × 3、arm64 × 1、备份脚本 × 1）都在源端与目的端各算一次 sha256 并比对一致；`install.sh` 装前还会实跑一次 `version`，架构不对就直接拒绝 |
| 备份与恢复演练（M7） | VERIFIED（生产实测） | `deploy/backup.sh` 在 `alienware-life` 真跑：归档 977/977 行、config 在位、sha256 自校验通过；再把归档解到临时目录用**生产二进制**跑 `doctor -net=false` 与 `deals -min 60`，恢复出的库与在线库四个数完全一致（`已见 640 · 已推送 248 · 游标 85 · 2100 KB`），`daily:last_sent` / `daily:watched_since` 游标也在 |
| 备份定时器与轮换 | VERIFIED（生产实测） | `systemctl start deal-hunter-backup.service` 在加固单元里真跑通（`978/978` 行、392K、`Finished`）；`list-timers` 下次触发 `2026-09-20 20:35:03 UTC` = 北京 04:35 —— **`OnCalendar` 必须写时区**，主机是 UTC，裸写 04:30 会变成北京中午；轮换在临时目录里以 `KEEP=2` 连跑三次验证，剩正好 2 对文件；归档落盘 0600（systemd 默认 umask 022 也压不住脚本里的 `umask 077`） | 第一版落盘 0644 而归档里装着 `deal-hunter.env`（webhook + 签名密钥）—— 脚本自己的校验把这次备份判成失败了，顺带暴露权限问题。现在 `umask 077`、文件 0600、目录 0750；实测过：`/var/backups/deal-hunter` 是 750 root:root，归档内 per-file 权限按原样保留（env 仍是 0640 root:dealhunter）。目录不可穿越，所以那两分钟内没有实际泄露面 |
| GitHub Actions | NOT APPLICABLE | 项目故意不使用（额度），门禁是本地脚本 + 办公室构建机 |

## 复核判据（下一条南昌公告出现时执行）

判据（两端各一条，通道为飞书 + OpenClaw drop，故每条各 2 行日志）：

- **开抢**：临近开抢 ≤45 分钟时收到 ⏰，标题带"开抢"，且同一行只收到一次。
- **到期**：临近截止 ≤3 小时（`expiry_lead`）时收到 ⏰，标题带"截止"；已过期的一律不发。
- 前提是**库里先得有带时刻的行**：`grep -c expires_at deals.jsonl` 目前为 0，若一直是 0，
  说明公告措辞仍解析不出来（去看 `dealhunter probe -source nc-vouchers-swj` 的详情正文），
  而不是提醒通道坏了。

```bash
ssh alienware-life 'sudo -u dealhunter /opt/deal-hunter/deal-hunter events -config /etc/deal-hunter/config.json'
# 面板地址只写在 root 可读的 env 里，别把字面量搬进仓库
ssh alienware-life 'B=$(sudo grep -m1 "^DH_SERVER_BIND=" /etc/deal-hunter/deal-hunter.env | cut -d= -f2-); sudo curl -s "http://$B/api/v1/status" | grep -A11 "\"event\""'  # due_opening / due_expiry / sent_today
ssh alienware-life 'sudo journalctl -u deal-hunter --since today | grep "kind=event"'               # 每个事件 2 行（两个通道）
ssh alienware-life 'sudo grep -oE "\"event:(reminded:[^\"]+|sent)\"[^}]*" /var/lib/deal-hunter/state.json'  # 没提醒过时为空（今天即为空）
ssh alienware-life 'echo -n "带时刻的行: "; sudo grep -c -e expires_at -e starts_at /var/lib/deal-hunter/deals.jsonl'
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

生产二进制戳 `3db4152`（时区修复那一版）。`origin/main` 与本地 HEAD 在它之上还有文档提交
（本文件所在提交），所以**代码戳 ≠ HEAD 哈希不代表部署落后** —— 要比的是"最后一次改代码的提交"。
判断办法：`git log -1 --format=%h -- '*.go' go.mod` 才是"运行中的二进制落后了没有"的正确问题 ——
今晚就出现过戳是 `e23a59f` 而最后一次改 Go 代码是 `25afd4c` 的情况（那一版只动了 deploy 与文档，
我们照最新提交打包，所以戳比代码新，不是落后）。用 `':!docs'` 会把 `deploy/` 的改动也算进"代码"，
在只改单元文件的提交上给出假阴性结论。

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
推送与部署被暂停，等真实用户确认后才继续（此后一路做到 `e23a59f`，`origin/main` 与生产戳一致）。
生产机上另有两个**不属于本会话**的残留：`/tmp/dh-m5-1789888881`（今天 07:21 的仓库快照，含
config/deploy/dist）与 `/tmp/dh-wire-feishu.sh`（9-19，把服务接到租户已有飞书应用的接线脚本）。
"m5" 这个命名本会话从未用过，怀疑有第二个会话或代理在同一台机器上并行操作本项目 —— 这也能解释
上面的伪造输出。**未删除、未改动它们**，下次动这台机器前先确认来源。

## Notes for the next session

- **在这台机器上跑 CLI 一律 `sudo -u dealhunter`**，不要 `sudo dealhunter`。今晚以 root 跑过一次
  `events`，`state.json` 变成 root 所有，服务立刻崩溃循环（`permission denied`），恢复要把文件
  chown 回去。`1209fe0` 之后只读命令不再写状态，但写类命令（`notify-test`、`compact`）仍会。
- **不要把 `install.sh` 的输出接进 `| head`**：远端脚本被 SIGPIPE 打断后会在"备份完、没重启完"
  的地方停下，看起来装好了其实服务还跑着旧版（今晚 `9daa3d3` 就是这样落后了一次）。要截断就先
  `> /tmp/x.log 2>&1` 再 `tail`，并确认 `installer rc=0`。
- **`pkill -f <名字>` 会连自己一起杀**：在远端用 `ssh host '… pkill -f dealhunter-linux-arm64 …; rm -rf …'`
  做冒烟测试时，模式出现在自己那条 `bash -c` 的命令行里，pkill 先把这个 shell 打死，后面的清理就没跑
  （kylin-pc 上一次留了 5 个临时文件，第二次手工清的）。要么用 pid 文件，要么把 pkill 放到最后一条独立
  ssh 里，并且用 `pgrep -fa '<确切命令>' | grep -v pgrep` 判断，别信 `pgrep -c`（它把自己的 shell 也数进去）。
- **`interval` 有下限校验**：`3s` 会被 `config: interval 3s is too aggressive; use >= 1m` 直接拒。
  想做快速冒烟，用 `1m` + 启动即跑的那一轮（`trigger=startup`），别改这个下限。
- **arm64 冒烟的做法**（`kylin-pc`，Kylin V10 SP1）：本地 `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build`
  → scp → `chmod +x`（Windows 侧传过去的模式可能是 0644，直接跑是"权限不够"）→ `doctor -net=false` +
  `run` + `curl /healthz`。全程在 `/tmp` 里，跑完删干净；**这台机器不是部署目标**。
- **`sudo cmd < file` 里的重定向不由 root 执行**：`sudo wc -l < /var/lib/deal-hunter/deals.jsonl`
  报的是"Permission denied"，因为 `<` 由调用者（非 root）打开。恢复演练第一次就跑在这上面。
  写成 `sudo wc -l <路径>` 或 `sudo sh -c '...'`。
- 备份落在 `alienware-life:/var/backups/deal-hunter`（0750 root、归档 0600）。**目前仍与生产同一块盘**：
  要真正抗整机故障，得设 `DH_BACKUP_PUSH=<另一台主机>:<路径>` 并写进 `/etc/deal-hunter/backup.env`，
  目标位置需要用户定（LIFE 域里可选的机器见基础设施记忆）。
- **解析器的修正不会追溯已经入库的行**：RSS/搜索类源靠游标只播新条目，所以 `rangeEnd` 修好之后，
  今晚那条已经存下来的 `Qwen3.8-Flash`（有 `starts_at`、缺 `expires_at`）仍不进日报，除非它再次
  被采集到。想要一次性回填，得先设计"重解析全库"的命令，别手改 `deals.jsonl`。
- **部署目标是 `alienware-life`**（SSH 里的名字，用户基础设施文档中 LIFE 域的 life-vm 角色）。
  `work-vm` 上从未装过本项目 —— 它是 LIFE 域服务，不要挪过去。
  二进制在 `/opt/deal-hunter/deal-hunter`（不是 `/usr/local/bin`），状态目录 `/var/lib/deal-hunter`，
  配置 `/etc/deal-hunter/config.json`；env 与 state 只有 root 可读，探测要带 `sudo`。
- 面板实际绑定地址来自 `/etc/deal-hunter/deal-hunter.env` 里的 `DH_SERVER_BIND`（当前是一台
  Tailscale mesh 地址），**覆盖** config.json 的 `server.bind`；所以在本机 `curl 127.0.0.1:8765`
  连不上不是故障，要用 env 里那个地址。
- 交付相关的状态键：`daily:last_sent`、`daily:watched_since`（进程启动即刷新）、
  `urgent:sent` 与 `event:sent`（都是 `{"date":"2006-01-02","n":1}`，按 `timezone` 的本地日记账）、
  `event:reminded:<fingerprint>`（每个事件一次；**开抢与到期共用这一个标记**，所以一张券不会说两遍）。
  `state.json` 里残留着已无人读取的 `digest:last_sent`，没有功能影响，故未动生产状态文件。
- 事件通道现在有两个窗口：`notify.event.lead_time`（开抢前）与 `notify.event.expiry_lead`
  （到期前，默认 3h，env `DH_EVENT_EXPIRY_LEAD`）。生产 `/etc/deal-hunter/config.json` 里
  `event` 块**部署这一版时要补 `expiry_lead`**，否则走内置默认 3h（行为一致，只是配置不自明）。
- 生产 `/etc/deal-hunter/config.json` 已清理：删掉 digest 与 feishu 的废弃键，显式写入
  `daily` / `urgent` / `event`，南昌两源放在 `extra_sources`；`openclaw.{command,args}`
  （sudo 包装脚本）原样保留。废弃的 `DH_MIN_SCORE` 之类已从生产配置移除；env 密钥未动。
- `notify.feishu.timezone` 现在只用于卡片页脚时间，与顶层 `timezone` 语义重叠。
- OpenClaw 的开关只有 JSON 里的 `notify.openclaw.enabled`，**没有** `DH_OPENCLAW_ENABLED` 环境变量。

## Next Candidate

ROADMAP 的 Next Candidates #1「到期前提醒」：与 M3 共用 `starts_at`、`Upcoming` 与事件调度，
是 M3 机制的延长线，最省事。
