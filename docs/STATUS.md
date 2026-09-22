# Deal-Hunter STATUS

> 给下一次会话用的恢复点。只写有证据的结论：VERIFIED / NOT VERIFIED / BLOCKED / NOT APPLICABLE。
> 长期方向看 [ROADMAP.md](ROADMAP.md)，用法看 [../README.md](../README.md)。

最近更新：2026-09-22。**生产跑的是哪个提交、怎么核**见下面「Current Version」——那里只给命令，
不给抄下来的哈希，因为抄下来的哈希每隔几个部署就是错的（这一篇就过期过一次）。

## 现在在等什么

1. **三个已经排上日期的真实窗口**（M3/M5 那件挂起项第一次有了具体时刻，判据在「复核判据」）：
   `09-27 23:59 截止`（93 分，到期窗口北京 20:59 起）、`09-30 23:59 截止`（三条同刻，
   一张卡装不下会由下一轮补发）、`10-20 06:00 开抢`。
2. **三件要人判的事**：清单/汇总类条目要不要继续进日报（量过：在效候选里 32 条）；备份要不要
   跨机（`DH_BACKUP_PUSH`，现在与生产同一块盘）；面板日间模式合不合口味（已经在无头浏览器里
   两态各截过一张图，浅色底墨字、对比度达标这件事不用再等工具，剩下的只是你喜不喜欢）。
3. 维护排程已经不需要外部定时器了（M19 把它改成重启不影响），但**第一次自动删数据**要等到
   盘上记号满 7 天：生产记号 `maint:last_compact = 2026-09-21T16:45:43Z`，所以最早
   `2026-09-28 16:45Z`（北京 09-29 00:45）那一轮会真的剪。要改主意的话在那之前撤掉即可；
   前一晚有夜间备份兜底。参数本身（7 天一剪 / 留 60 天）合不合适也还没量过第二样本。

里程碑逐个做了什么、留下什么证据，都在 ROADMAP 的里程碑表（M0–M17）里；本文只写**当前状态、
验证结论、下一次该接手什么**。原先这里抄了 2026-09-20 那几节的 M3/M5 叙述，已经删掉 ——
它们当时就写着"生产仍为 `3db4152`"，而那是七个部署之前的事情。

## Verification

| 项 | 结论 | 证据 |
|---|---|---|
| 压缩排程以备份为前置（M29） | VERIFIED | 排程要删行，就必须在「删了还能捞回来」的证据下才动手：`backup.stamp` 缺失 / 为空 / 首字段不是时刻 / 距今超过 `store.BackupPatience`(36h) 四种都跳过并 Warn。36h 这条线此前只有 doctor 一份实现，再加消费者就会各写一份，故收进 `store.BackupAge`（返回「多久前 + 归档名 + 错误」），doctor 与排程共用；`ErrNoBackupRecord` 让 doctor 仍能区分「从没跑过」（提示 enable 定时器）与「跑了但记号坏了」。**手动 `dealhunter compact` 不受此限** —— 那是人明确下的令，不该被门禁挡。表驱动 5 个子用例（4 拒绝 + 1 必须真剪的对照），首版我把断言写反（`kept != present`），五条一起红才看出来是断言错不是实现错；四条反向对照各红该红处。生产侧核对：当前记号约 12 小时前，所以 09-28 那次自动剪枝会照常进行 |
| 文档表格形状有门禁（M28） | VERIFIED | 起因是我自己今晚两次把表格改坏（中间留空行、少写一列），加上上一轮偶然发现的 M16/M17 两行被物理换行切断 —— 三种都逃过 `grep '^\|'`。测试按 GFM 逐块核对：表头、紧跟的分隔行、之后每行的未转义竖线数必须等于表头；单行表格块直接判错（那通常就是断行的残段）。转义 `\|` 按内容算，所以带竖线的代码片段不会误报。当前 **12 个表格块全部合规**。两条反向对照重放真实错误：插空行 → 三处红并给行号；少一列 → 红并打出 "3 个竖线，表头是 4 个" |
| 采集器种类与文档、计数对表（M27） | VERIFIED | 真来源是 `config.go` 里的 `Kind*` 常量（正则读回，不是手抄），要求：每种都能被 `sources.New` 构造且 `Kind()` 回原值、README 采集器表里有对应行、ROADMAP 那句"采集器类型有 N 种"的 N 等于实际数目。**今天三处一致（7 种），无漂移** —— 这条的价值在将来，且它取代的旧测试是手抄常量列表（加一种不会报警）。四条反向对照各红该红处，其中"新增 `KindGraphQL` 但不接线"一次点亮三条臂（造不出来 / README 没有 / 数目不符），证明三条都是活的；最后一条把真来源读空 → 红在 "only 0 Kind constants read back" |
| 出厂 env 样例覆盖全部 `DH_*` 开关（M26） | VERIFIED | 双向对表：config.go 里 `"DH_..."` 常量的集合 ⊆ 样例里的赋值集合，且反向也成立。今天补掉三条缺口（`DH_FEISHU_API_BASE`、`DH_CONFIG`、`DH_ENV_FILE`）。**规则被自己的反例纠正过一次**：第一版把散文里的 `DH_MIN_SCORE` 误报成缺口，而那句是"这个开关曾经没用、已删除"的历史说明 —— 收紧为"只有赋值（含注释掉的赋值）算文档"。三条反向对照各红一处：删真赋值、加代码不认的赋值、以及最要紧的第三条 —— **删掉赋值只留散文提及，仍然红**，否则"提到就算文档"会把这条门禁掏空 |
| 出厂配置的每个键都真实存在（M25） | VERIFIED | 反射列出 `Config` 接受的键路径（`json:"-"` 不在内，所以 `webhook_url` 这类密钥形状的键会被当成未知键报出来），逐路径核对 `config/deal-hunter.example.json` 与 `.starter.json`，并要求 `Validate()` 过。**今天的结论是没有漂移**（除有意的 `_comment`），所以这条是防将来的：改 tag 的人不必记得去改例子。首跑即绿，故补四条反向对照各红一处：`commnd`、`webhook_url`、结构体 tag 改名（两份配置同时红在同一路径 `notify.event.expiry_lead`）、真来源读错类型 → 红在防空转守卫（打出 "only 18 keys read back"）。同一轮顺手核了 `deploy/deal-hunter.service` 与生产 `/etc/systemd/system/` 里那份**逐字一致**（`diff` 无输出），所以"仓库里的单元就是生产在跑的"这句今天仍然成立 |
| 文档里的 CLI 旗标对表（M24） | VERIFIED | 新测试从 main.go 读回每个 FlagSet 声明的旗标，核对 README / docs / 门禁脚本里 `dealhunter <命令> -旗标` 的每一处；当前样本 9 处、0 处不存在（判决行会打出被检查的 `文件 -> 命令 -旗标` 元组，以及该命令实际声明的旗标清单）。**过程里两次自我纠正**：① 命令识别最初用「有没有 FlagSet」，导致 `events`/`serve`/`sources` 等无旗标命令整行不可见，而那正是 `-config`/`-data` 出现的地方（M21 记的那条）—— 改成从 dispatch 表取命令集合，样本 7→9；② 第一次反向对照我把它做成了存活（假旗标写在命令清单的描述行，超出门禁声明的范围），换到真调用行才红。另外三条对照各红一处：`-nett` 红在判决、产品改名 `-net` 红在文档侧、全局 FlagSet 改名红在「防空转」守卫本身 |
| 三份命令清单对表（M23） | VERIFIED | 真来源是 `dispatch` 表，测试用 `go/parser` 从 AST 读回它的键，再要求 usage 与 README 各列出同一批名字（两个方向都查）。首跑即抓到一条真漂移：README 的命令清单少了 `version`。四条反向对照：只加进表 → 红两条；usage 少写 → 红 usage 那条；README 少写 → 红 README 那条；文档里放一个不存在的 `ghostcmd` → 红在反方向那条。原先那两条手抄命令名（`events`、`reparse`）已删 —— 它们只能证明"这两个字出现过"，不能证明清单齐。 |
| 门禁真的起一次监听（M22） | VERIFIED（两份孪生脚本各过反向对照） | 接口测试全走 `httptest`，只到 handler；`Serve()` 的绑定 / `enabled` 闸门 / 公网拒绝 / `-hold` 退出这四件事以前没有任何自动化步骤跑过。现在门禁会真起一次（端口让内核挑、从日志读回，不猜固定端口），断言四个端点 200、面板 HTML 的 `data-theme` 计数、`live_in_briefing` 在位、公网绑定必须在监听前被拒、到点自行停止且日志无 ERROR。**反向对照（改产品，不是改断言）**：`Serve` 不监听 ⇒ "never reported a listening URL"（bash 与 PS 各一次）；`checkBind` 公网分支改成 `return nil` ⇒ "a public bind started without complaint"（PS 那次是产品级；bash 另用 `DH_ALLOW_PUBLIC_BIND=1` 验同一臂）。两份脚本的一处差异是环境造成的、已写进注释：PS 的 `Start-Process -PassThru` 取不到 `ExitCode`（实测只有 `-Wait` 有，而 `-Wait` 期间无法探测），所以"干净退出"只在 bash 断言，PS 改断"到点不再应答 + 无 ERROR"。顺带修掉两处 PS 自身的坑：`-WindowStyle Hidden` 与 `-NoNewWindow` 互斥（第一次探针全都没起进程，读到的空 ExitCode 是假信号）、`$serveProc` 为 null 时 `Stop-Process -Id` 会二次报错盖掉真因 |
| 读路径在**稳态规模**下的成本（M21 附带） | VERIFIED（合成实测） | 旧那句"量过，不是债"是在 779 行上量的，而 M19 之后库会稳定在 60 天 ≈ 7200 行 —— 所以拿合成库（7200 行 / 2.5 MB，真实字段分布）重测一次：`/api/v1/status` `0.55–0.69s`、`deals?limit=24` `0.17–0.20s`、`sources`/`runs` `~2ms`；同一二进制换 20 行库 `status` `3–6ms` ⇒ 成本 = 每行 ~0.08ms × 每请求 3 次全扫（`Live` + `Upcoming` + `Expiring`）。面板 30s 一轮 ≈ 单核 2%，首屏异步。**结论是不做缓存**（两个进程都写这个文件，M17 就是为此存在；缓存一致性是新故障，换来的只是几百毫秒）。过程中先读到过一次 `status 2.05s` —— 那是**连接被拒的超时**，不是慢查询：计时之前必须先确认监听真的在（`healthz` 200）。数字与复测办法都写进 ROADMAP 的 Technical Debt 那条。 |
| 死代码与两条从没跑过的采集支路（M21） | VERIFIED | 用 `go test -coverprofile -coverpkg=./internal/...,./cmd/...` 横扫 0% 函数（23 个），逐个查引用后**删掉 7 个没有任何调用者的函数**：`scoring.discountPct`（恒等函数）、`notify.min`（Go 1.21 起内建）、`Feishu.WebhookHost`、`sources.OfficialHosts`、`model.Deal.Kinds`、`FileDrop.Dir`、`base.IsOfficial`（Source 接口里没有它，管线用的是 `IsOfficialURL`）。判据是"非测试引用数 = 定义那一行"，删完 `go build ./... && go test ./...` 全绿 = 确实无人依赖。剩下的 16 个 0% 里 12 个是进程接线（`main`/`cmdRun`/`cmdServe`…），只有两条是**真在生产跑却没测过的配置支路**，各补一条测试并配三次定向变异：`snapshot` 的 `mode=json`（`flattenJSON` 拍平嵌套价格字段，红在"事实必须带 JSON 路径"那条上）、`html` 的 `class_contains`（`focusOnClass` 是**每个命中 4000 字节的窗口**，不是"只留那个元素"；三次变异分别红在"没过滤"“过滤过头""窗口宽到什么都没挡"）。补测过程里两次被自己的夹具骗到：远那一行原本 8 个字，先被 `min_text_len=10` 挡掉，看起来像 focus 生效；值的裸长度 7 字时"去掉路径"会红在"没有事实"而不是红在路径断言上 —— 两处都把夹具加长，让红落在真正主张的那一条。 |
| doctor 不再可能把 webhook token 打在终端上（M21） | VERIFIED | 这条承诺 API 侧早有测试（`TestNoCredentialEverLeavesTheProcess`），**CLI 侧一直没有**，而截断逻辑就写在 doctor 里。现在 `TestDoctorPrintsOnlyTheHostOfAWebhook` 钉三样：主机名要在、token/签名密钥/路径前缀都不能在。把 doctor 的截断整段删掉 ⇒ 两条 leaked 断言变红（含 `/open-apis/bot/` 那条，它专门抓"忘了截断但记得打码 token"这种半吊子）。 |
| 压缩记号写失败不再静默（M21） | VERIFIED（新增告警） | 追一个测试 flake 时发现的：`compact()` 里 `_ = st.PutState(...)` 吞掉了写失败，于是"剪过了但没记号"这种状态只能从症状反推。现在失败会 `Warn("compact marker not written")` 并提前返回。flake 本身是 Windows 的：轮询 `state.json` 正赶上服务 rename，拿到共享冲突被我的测试当致命错误 —— 改成"读不到就当还没发生，继续轮询到 deadline"，并把循环自己的日志带进失败消息（否则下一次只能看见"没记号"看不见原因）。三次全量并发跑复现过，改后连跑三轮干净。 |
| 解析漂移变成常驻读数（M20） | VERIFIED | 起因是今晚手算了一次"当前解析器比库里多读得出多少行"（生产 872 行，`expires_at` 18 行、`starts_at` 3 行，漂移 **0**）。这种数本来不该靠人记得去算：`doctor` 现在自己报一行 `reparse drift`，并钉住**它报的数必须与 `dealhunter reparse` 的 dry-run 同数**（不一致就说明其中一个是空转）。写回**没有**进排程 —— 与 M19 不同，覆盖一条已写明的截止时刻可能把真值改错，风险等级不一样，所以仍然人跑 `reparse` 看过差异再 `-write`。`cmdReparse` 的逐行判定抽成 `reparseRow`，两处共用（CLI 与 doctor），原五条 reparse 测试不改一字仍然通过 = 抽取没动语义。新测试首跑即绿，所以补了五次定向变异：永远报 ok / 把"检查过几行"当成"漂移几行" / doctor 不加这行 / ok 文案不再报检查数 / 把截止槽位读成开抢 —— 各红该红的那一条（其中最后一次红在"两边必须同数"那条交叉对照上，正好证明交叉对照有用） |
| 排程压缩不再被重启清零（M19） | VERIFIED（测试 + 生产的"该不该剪"那一半） / NOT VERIFIED（生产的"真剪"那一半） | 触发器原来数的是**进程内**轮次（`rounds%48`），天天部署的机器永远凑不满 —— 这就是 Known Risks 里"生产跑了一整天而 `maint:last_compact` 不存在"的成因。现在每轮都调 `compact()`，节流只看盘上记号（≥7 天）。`internal/scheduler/scheduler_test.go` 是这包的**第一份**测试（原来一行都没有）：剪得掉旧低分行且不动新行、24 小时内不重剪、**启动后第一轮就剪**（间隔设成 24 小时，所以旧代码必然凑不满 48 轮；轮询到记号出现为止，不是掐秒表）、记号读不出当作"该剪"而不是"刚剪过"。四条各配一次定向变异：去掉节流→只红第二条，去掉首轮检查→只红第三条，剪完不写记号→红第一三条与第四条，把"读不出"当刚剪过→只红第四条。生产 `124b2b6` 部署后的读数：`trigger=startup … errors=0` 之后**没有** `store compacted` 行，记号仍是 `2026-09-21T16:45:43Z`（未满 7 天，节流如期），`doctor` 那行 `✓ compaction 上次压缩 12.0小时前`。**"真的剪了一次"要等 `2026-09-28 16:45Z`（记号满 7 天）那一轮**，见「现在在等什么」#3 |
| 资源占用（本轮实测） | VERIFIED（生产读数） | `systemctl show deal-hunter`：`MemoryCurrent=16,830,464`（16.8 MB）、`MemoryPeak=21,708,800`、`TasksCurrent=8`；状态目录 2.0 MB（`deals.jsonl` 868 行 / 1.38 MB、`state.json` 22.5 KB / 111 键），一夜 +119 行。面板轮询侧的开销先前已量过（`status` 0.118s、`deals` 0.039s/97KB，30 秒一轮 ≈ 0.4% 单核）。**结论：没有需要优化的东西**，磁盘增长由 M19 的排程兜住 |
| 信源静默判据的语义（M16 补正） | VERIFIED（生产实测） | 上一版用 `found`（**过闸后**的行数）判断源是否还活着，会把"页面照常出条目、今天没有一条含关键词"误报成衰减。现在记的是闸门**之前**的条数（`SourceReport.Parsed`，由 `base.deal` 计数、`Source.RawSeen()` 暴露，八个采集器靠嵌入提升零改动）。生产 2026-09-22 的对照读数：`nc-vouchers-zf` 解析 54 → 命中 0、`nc-vouchers-swj` 143 → 1、`lowendtalk-latest` 97 → 80 —— 前两个在旧语义下都会在 36 小时后被点名，现在每轮刷新记号，安静源从 3 降到 2。第二天又补一刀：`openrouter`/`snapshot` 的计数还在**自己那层差分** 之后（新增免费模型、价格事实有变），所以部署 `121a027` 后改成数"看到的行"（实测 parsed=443 与 8、 found 都是 0），没记号的源 **17 个里 0 个** —— 实时读数看 `/api/v1/sources` 的 `last_hit`，别抄这里。面板并排显示"解析 N · 命中 M"，这个区别可诊断而不是要靠读代码推 |
| 退役状态键真的从盘上消失了 | VERIFIED（生产实测） | `digest:` 命名空间在装载与"以磁盘为底的合并"两侧都过滤（缺一侧就会被另一侧捞回来，各配一次变异）。部署 `b5c528a` 后 `grep -c 'digest:' state.json` = **0**，而键总数 110、`maint:last_compact` 仍在（9.6 小时前那次手动压缩的记号又活过一轮） |
| 单元 + 集成测试 | VERIFIED | `go test -race ./...` 本地与 linux-ci 均全绿。测试函数数量不在这里抄数（抄一次就开始漂）：现算 `grep -rn "^func Test" --include=*_test.go . \| wc -l` |
| 快速门禁 | VERIFIED | `bash scripts/ci-local.sh --quick` → `✓ CI PASSED e849a0b`（fmt/vet/build/secretscan/身份/离线冒烟） |
| 全量门禁 + 干净环境 | VERIFIED | `DH_CI_HOST=linux-ci bash scripts/ci-office.sh` → 判决行 `✓ office CI passed (linux-ci, N files)`（N 取自该次运行，别抄），解包到 `/tmp/deal-hunter-ci-*` 全新目录 |
| 三平台产物 | VERIFIED | 交叉编译 linux/amd64、linux/arm64、windows/amd64 全过；arm64 产物已在真机跑过（见下一行），amd64 产物跑在生产机 |
| 事件提醒端到端（夹具驱动） | VERIFIED | 窗口内恰好一次、跨重启不重发、窗口外（提前 3 天 / 迟到 2 小时）静默、三条渲染路径都带时刻；窗口守卫经"临时翻宽→变红→还原"验证不是空过 |
| 真实公告措辞解析 | VERIFIED | `9月19日9:00至9月26日24:00`、`上午9:00开放`、`2026年9月19日9:00起` 均正确；`24:00` 拒绝当作开抢时刻；`9月31日` 被 round-trip 守卫拒绝（否则 `time.Date` 会滚到 10-01，猜成未来就真会发错提醒） |
| 截止时刻的时区（原候选 #5） | VERIFIED | `ExpiresAt` 不再用 `time.Local`，改用 `scoring.Input.ReaderZone`（`StartsIn` 改名，含义覆盖开抢与截止两端）。守卫写法是"同一钟表在 UTC 与 UTC+8 必须差出整 8 小时"，所以与测试主机所在时区无关；把 keywords 层与 scoring 层的传参分别改回旧写法，两条测试各自变红已验证 |
| 南昌信源可达性 | VERIFIED | 从生产机出口实测：商务局列表页与详情页 200/UTF-8/SSR；市政府列表页 200/32KB；`/ncszf/tzgg/` 307 跳首页（所以必须配 `2021_nav_list.shtml`）；`tyj.jiangxi.gov.cn` 用 curl 因不支持 legacy 重协商而失败，但 Go TLS 可过 |
| 生产部署 | VERIFIED | `deal-hunter e23a59f · built=2026-09-20T17:16:15Z`（= `git log -1 -- ':!docs'`）；今晚四次部署（`378fd8d`→`9daa3d3`→`1209fe0`→`e23a59f`）产物 sha256 在构建机 / 本机暂存 / 生产三处逐次一致；`healthz` 200，17 源，首轮 0 错误，备份各留最近 3 份 |
| 只读命令不再换掉状态文件所有者 | VERIFIED（生产实测） | 修复前后各跑一次 `sudo dealhunter events` + `doctor`：`state.json` 的所有者与 mtime 都不再变动（修复前它会变成 `root:root`，服务下次写状态即 permission denied 进入崩溃循环 —— 今晚真实发生并恢复过一次） |
| 个人邮箱不再能溜进文件内容（M6） | VERIFIED | `internal/secretlint` 加 `consumer_mailbox`（`check-identity` 只看提交作者，正文里写死地址此前过得了所有闸）。两条夹具：命中即报（文档行与代码行各一）、同行 `secretlint:ignore` + 理由可豁免。变异复查过：把规则改名即"reported 0 findings"变红。**顺带证明门禁自身有效**：加规则的当场，`TestRepositoryIsOpenSourceClean` 就把我的假邮箱夹具拦下了，按既有惯例用带理由的标记放行 |
| 时区修正的可观察效果 | NOT APPLICABLE（暂无对象） | 生产库 904 行带 `meta`，但 **`expires_at` / `starts_at` 均为 0 行** —— 至今没有任何一行真的解析出了时刻并落库（4 月那条在落库前就被拒）。所以这次修正在生产上暂无可比对的观测面，效果只由测试与两道变异守卫证明。第一条带时刻的行落库时要回头看一眼偏移是不是 `+08:00` |
| 真机观察：券公告触发行为 | VERIFIED（观察本身） | 南昌两源：商务局源那一条是 4 月的《如何领取养老服务消费券》问答，被 14 天年龄闸门与 `event.min_score` 双重挡下；市政府源当天 `found=0`，直取该页 200/32KB 可见行全是"拟补贴对象公示""换新补贴明细"这类，**没有一条含券关键词** —— 两种"抓到但拒收"都是设计行为。判据要记牢：**`found` 统计的是过闸后的行数，0 不等于解析失效**（这条后来被写进代码：`SourceReport.Parsed` 是闸门前的条数，健康度用它，不用 `Found`）。事件通道当时 0 发 0 候补，无误发无骚扰 |
| **真实"到时候提醒"实地复核（开抢与到期两端）** | **NOT VERIFIED** | 今天南昌没有"写了明确时刻且临近开抢或截止"的市级公告；唯一在闸内的行是 4 月问答。省级在跑的（赣超 / 体育消费券 9-14 起、每人每周限领 2 张）用的是**周期性/无年份**措辞，M3 故意不猜。触发路径已由夹具与真实措辞两侧证明，等的只是一条现实公告 |
| 到期前提醒（M5，夹具驱动） | VERIFIED | 窗口内（2h / lead 3h）恰好一次且跨轮不重发；还有 5 天与已过期 30 分钟都静默；`max_items=2` 截断后第三行不留标记；标题带"截止"；只有截止时刻的行进 `events` 清单与 `due_expiry` 计数。四条守卫逐个变异复查过（把实现改回旧写法即红） |
| 起止区间写法的截止（生产真丢过一条） | VERIFIED | 部署 `378fd8d` 后 `dealhunter events` 打出生产第一条带时刻的行：`09-18 10:00 开抢 … 89 分 Qwen3.8-Flash`，而 `expires_at` 为空 —— 原文是「新加坡时间：2026年9月18日10:00至2026年9月30日23:59」，区间没有"截止"前置词，于是按 M3 口径整条掉出日报。加 `rangeEnd`（两个小正则：区间前后各须一个完整日期）后修复。端到端守卫 `TestBriefingKeepsADatedEventWrittenAsARange` 与单元守卫都做过变异复查：短路 `rangeEnd` 即"got 0 rows" |
| 配色三态切换（M18） | VERIFIED（结构 + 无头浏览器实测） | 旧写法点一次就把 `light`/`dark` 写进 localStorage，**"跟随系统"从此回不去**，傍晚系统转深色而面板不动。现在是 `跟随系统 → 日间 → 夜间 → 跟随系统` 三态循环：自动态用 `removeItem` 撤销显式选择，首屏与点击共用同一条 `dhIsLight(dhThemeMode())` 判定，另挂 `matchMedia` 的 `change` 监听。结构守卫 6 条逐个变异复查（去掉 removeItem / 去掉监听 / 循环塌回两态 / 自动态不读系统偏好 / 首屏自己另写一套判定 / 按钮又改成显示"点下去会到哪"），**每个都只红一条**。浏览器侧用 CDP 打在临时的 `serve` 上（无头）：三态循环每步的 label、`localStorage`、`data-theme`、`getComputedStyle(body).backgroundColor` 逐点记录；把模拟的 `prefers-color-scheme` 从 light 翻到 dark，**不刷新**，面板当场转暗而 storage 仍是空的（监听器真的跑到了），把模式钉成夜间后再翻系统则不动（"只有自动态听系统"也看到了）。顺带改文案：自动态原本写"点击切到日间"，可系统本来就是日间时那看起来像按钮坏了 |
| 空库不假装有日期（M18） | VERIFIED | `/api/v1/status` 的 `store.oldest` / `store.newest` 在从没跑过时序列化成 `0001-01-01T00:00:00Z`，面板照原样打印成"最近 1/1/1 08:05:43"（截图里看见的）。现在零值给 `""`，面板那行变成"最近 —"。守卫两侧都有牙：去掉零值判断 → 空库那半红；把助手改成恒空 → 跑过一轮那半红（`parsing time "" …`），两个方向的变异各红一次 |
| 面板日间模式（M5） | VERIFIED（结构） / 观感已由 M18 在浏览器里看过 | 结构守卫：浅色块必须重定义 `--bg/--panel/--txt/--dim/--line`，暗与亮两套 `--txt` 对 `--bg` 的对比度都 ≥4.5:1（测试里现算 WCAG），且不允许残留 `rgba(255,255,255` 硬编码。配色好不好看**没有工具能证明**，但"浅色底配墨字是否真的渲染出来了"有：见上一行的 CDP 读数与两态截图 |
| 面板在采集轮进行中应答（M4） | VERIFIED | 单元层：把信息源卡在请求里，读取方原本等满 3 秒，拆锁后立刻返回，`-race` 干净。生产层：重启后 40 次打 `/api/v1/status` 最大 168ms（1 次非 200 是重启缝隙，稳定态复测 25 次全 200、最大 105ms） |
| ARM64 实跑 | VERIFIED | 2026-09-21 在 `kylin-pc`（Kylin V10 SP1，`Linux aarch64`）跑 `25afd4c` 交叉编译产物：`file` 确认 ELF aarch64 静态；sha256 `ede936fd…` 两端一致；`version` 报 `go1.26.4/linux-arm64`；`doctor -net=false` 体检通过；`run` 真实起了一轮（`sources=15 new=67 stored=67 errors=0 took=4.4s`）；面板在 127.0.0.1 上 `healthz=200`、`/` 里 `data-theme` 命中 6 次。**注意**：这是临时冒烟测试，没有装 systemd 单元，kylin-pc 也不是本项目的部署目标（部署仍在 `alienware-life`） |
| 校验和与传输完整性 | VERIFIED | 今晚五次产物传输（amd64 × 3、arm64 × 1、备份脚本 × 1）都在源端与目的端各算一次 sha256 并比对一致；`install.sh` 装前还会实跑一次 `version`，架构不对就直接拒绝 |
| 嵌入时区库的代价与效果 | VERIFIED（实测） | 生产主机上量到二进制 8,478,882 → 对比上一版备份 8,065,186 = **+413,696 字节**；换到的是 `-trimpath` 产物在 Windows 上从 `rc=2 unknown time zone Asia/Shanghai` 变成 `rc=0`（改动前那个坑是静默退回主机钟）。旧 `notify.feishu.timezone` 键在部署时仍留在生产配置里，`doctor` 与运行都不受影响（惰性废弃键），本次顺手从 `/etc/deal-hunter/config.json` 删掉并重启确认 `next_due` 仍是 `+08:00` |
| 备份与恢复演练（M7） | VERIFIED（生产实测） | `deploy/backup.sh` 在 `alienware-life` 真跑：归档 977/977 行、config 在位、sha256 自校验通过；再把归档解到临时目录用**生产二进制**跑 `doctor -net=false` 与 `deals -min 60`，恢复出的库与在线库四个数完全一致（`已见 640 · 已推送 248 · 游标 85 · 2100 KB`），`daily:last_sent` / `daily:watched_since` 游标也在 |
| 搜索引擎的人机验证不再伪装成"没结果" | VERIFIED（夹具） / NOT VERIFIED（生产） | `lite.duckduckgo.com/lite/` 对生产出口 IP 回了 **HTTP 200 + 验证页**（实测：14KB、内含 `anomaly-modal` / `cc=botnet` / `challenge-form`，0 条结果），采集器原本把它归进"本轮无结果"，面板上看起来像羊毛断了。现在 `search` 源会报 `demanded a human check` 进 `status.sources[].err`。两个方向都变异复查过：检测短路 → 变红；正则误伤正常结果页 → 也变红（夹具用的真实验证页去掉了逐请求签名）。**但 `8076158` 上线后那一轮 `status` 里 human check 命中 0 次** —— 验证窗口在部署前自己解除了，所以这条目前只有夹具证据，等它下次真挡我们时才算生产验证过。**这一条在 2026-09-22 14:34Z 补上了生产证据**：`fdb7c24` 部署后那一轮 `errors=4`，四个 search 源全部报 `search engine demanded a human check`（查询词「限时免费 大模型 官方公告」，并写明 no results this round），`/api/v1/sources` 的 `last.err` 与 `parsed=0/found=0` 同时可见 —— 也就是说「页面照常返回、但一条都没解析出来」这件事，检测器认得、也说得出原因。同一轮另有 1 条 `openrouter-free-models: context canceled`，那是旧进程被优雅停止时取消上下文造成的，不是故障。间歇性也再次量到：同一批源的 `idle_hours` 只有 1.08 小时，说明前一小时它们还正常出过内容 —— 结论不变：不为此改采集策略 |
| 边界清扫（时钟与预算） | VERIFIED | 四条表驱动测试：`max_per_day: 0` 现在真的是 0（以前 `budgetLeft` 与 `urgentMax` 各把它抬成 1，静音写法变成每天一条惊喜）；预算按**读者本地日**跨日（北京 23:59 花完、00:01 补回，同一个瞬间用 UTC 表达结论不变）；日报到点判定（08:59:59 不发 / 09:00:00 发 / 发过不再发 / 启动时时段已过不补发 / 明天的仍发）；提醒窗口四条边。**其中一条抓到真 bug**：截止时刻正好等于本轮"现在"也会提醒一次，那一刻已无事可做 —— 到期侧改为严格未来（`when.After(now)`），开抢侧保留迟到宽容（十分钟前的开抢还能领）。两条新守卫都变异复查过（改回旧写法各自变红） |
| 无年份截止 + 紧贴的时刻（M10） | VERIFIED | `ExpiresAt(text, anchor)` 的锚点带两件事：读者钟点、以及补年用的年份。规则保守 —— 同一年读法还没过期才接受；过期时只在年底附近滚一年且不超过锚点后 45 天（9 月的帖子写"截止至8月28日"是已经过了，不是明年）。日期后面紧贴着写了点就按点记（`截止到9月20日凌晨5:40` → 05:40），`24:00` 归到当天末尾；**逗号之后是另一句**，"至9月25日，10:00 开抢"不会把开抢时刻借成截止时刻，这一条单独有断言。用同一份生产语料复测：读数 28 行（含 3 行无年份），抽查两条一条从 23:59 变成 05:40、另一条本来就是真截止，没引入误报。`clockAfter` 做过变异复查（短路即变红）。顺手把 `expiryLead` 前置词表收成一个常量 —— 之前规则表和 `yearlessLeadRe` 各抄一份，改一处就会漂 |
| 用生产历史语料回灌解析器（M9） | VERIFIED | 从 `alienware-life:/var/lib/deal-hunter/deals.jsonl` 取 1040 行真实公告文本（md5 两端一致，只在构建机的一次性工作区里跑，明细不回传本地），喂给当时的解析器：64 行含日期字样，**只有 5 行读得出截止**。补了两类年份明写的写法（`开放至/持续至/顺延至/延期至/延长至/免费至/限免至` 允许前置词与日期之间最多 6 个非数字；`2026 年 12 月 31 日前` 这类"年在内 + 前"后缀，含空格变体）之后 → **24 行读得出截止**，另加抽查输出：11 个去重的"读到的值 ← 原句"全部是真截止（含 `活动持续至2029年3月31日`、`本期活动截至 2026 年 12 月 31 日`、区间那条），**没有一个把发布日/数据时效读成截止**；反向守卫 `TestExpiresStillRefusesPublicationDates` 就是钉这一点的。剩下的读不出里最大一类是不写年份的 `截止至8月28日`，已作为候选 #1 排队（需要发布日锚点 + 定一个跨度上限）。新规则做过变异复查：删掉前置词与"前"规则，2 条断言立刻变红 |
| 评分台阶与被忽略的时钟参数 | VERIFIED | 新鲜度台阶表驱动测试（24h / 72h / 7d / 30d 各两侧 + 时间戳超前按新算）全绿，实现没有 bug；但清扫本身抓到两件事：① `evaluate()` 的第 4 个参数原本**被忽略**，有一条老测试（`TestStaleItemIsPenalized`）把条目的 `PublishedAt` 当"现在"传进去，因为参数是死的才一直通过 —— 参数改名 `now` 并真正接上后它立刻变红，说明那条断言此前在空转；② 精确压在 30 天边界的用例会因"外层 now 早于内层 `time.Now()`"的亚秒漂移过档，所以边界测试必须自带时钟（生产无害：采集间隔 30 分钟）。上限测试做了非空转证明：去掉 `clamp` 后同一夹具有 **103 分** |
| 单一读者时钟（M8） | VERIFIED | 顶层 `timezone` 现在是唯一开关：写错（`Mars/Nowhere`）在 `config.Validate()` 就拒绝（以前只有 `notify.feishu.timezone` 会报错、顶层这个静默退回主机钟 —— 服务在 UTC 上就等于排程差 8 小时）；`notify.feishu.timezone` 字段已删除，`NewFeishu(cfg, loc)` 由 pipeline 传入同一个 `a.loc()`。守卫测试断言"东八区页脚 09:25 / UTC 页脚 01:25"，把 `tz := clock` 改回 `time.Local` 即变红。旧配置里残留的 `notify.feishu.timezone` 键变成惰性废弃键（有测试守着它不再泄漏进钟） |
| 只为测试存在的接缝已删（M8） | VERIFIED | `notify.DealsOf` 与 `var _ = notify.DealsOf` 一起删掉（`m.Deals` 本就是导出字段，测试直接用）；顺带清掉因此不再需要的 `model` 导入。同批修掉 README 配置样例里 M2 就删除的 `filter.min_score` 假旋钮，并补上缺的 `event` 块 |
| 备份定时器在加固单元里跑通（M7） | VERIFIED（生产实测） | `systemctl start deal-hunter-backup.service` 在加固单元里真跑通（`978/978` 行、392K、`Finished`）；`list-timers` 下次触发 `2026-09-20 20:35:03 UTC` = 北京 04:35 —— **`OnCalendar` 必须写时区**，主机是 UTC，裸写 04:30 会变成北京中午；轮换在临时目录里以 `KEEP=2` 连跑三次验证，剩正好 2 对文件；归档落盘 0600（systemd 默认 umask 022 也压不住脚本里的 `umask 077`）。第一版落盘 0644 而归档里装着 `deal-hunter.env`（webhook + 签名密钥）—— 脚本自己的校验把这次备份判成失败了，顺带暴露权限问题。现在 `umask 077`、文件 0600、目录 0750；实测过：`/var/backups/deal-hunter` 是 750 root:root，归档内 per-file 权限按原样保留（env 仍是 0640 root:dealhunter）。目录不可穿越，所以那两分钟内没有实际泄露面 |
| GitHub Actions | NOT APPLICABLE | 项目故意不使用（额度），门禁是本地脚本 + 办公室构建机 |

## 复核判据（下一条南昌公告出现时执行）

判据（两端各一条，通道为飞书 + OpenClaw drop，故每条各 2 行日志）：

- **开抢**：临近开抢 ≤45 分钟时收到 ⏰，标题带"开抢"，且同一行只收到一次。
- **到期**：临近截止 ≤3 小时（`expiry_lead`）时收到 ⏰，标题带"截止"；已过期的一律不发。
- **同一时刻挤多条时该看到什么**（09-30 就是这个形状：4 条都在 23:59，而 `max_items: 3`、
  `max_per_day: 2`）：第一张卡带最前的 3 条，**剩下那条不算丢** —— 它没被标记，30 分钟后的
  下一轮会单独再发一张卡；此后预算用完，再有第三条就只能等第二天（而窗口已过，等于不发）。
  这套"下一轮补发"原先只是 `TestDueEventsOrderOpeningBeforeLaterDeadline` 注释里的一句话，
  现在由 `TestEventOverflowIsCarriedByTheNextRound` 跑三轮钉死（撤掉 `max_items` 截断、
  拆掉预算闸门、把标记提到发送之前 —— 三种变异都变红）。
  **提前记下免得临场误判**：09-30 那张卡里预计会出现**两条《我享云》**（同一赞助商换措辞重发的
  两个 v2ex 帖，措辞差在"比矿泉水还便宜"与"一瓶矿泉水/月"）。这是刻意不折叠的（见 ROADMAP
  「同一场赞助活动换措辞重发」那条），不是当天出的故障 —— 判"对不对"看的是**每个 fingerprint
  只提醒一次**，不是"每个产品只出现一次"。
- 前提是**库里先得有带时刻的行**：`reparse` 落盘后（2026-09-21，生产 735 条去重记录）带截止的
  15 行、带开抢的 3 行；其中 6 行的截止已经过了，正是这 6 行此前一直混在"在效"里。判据命令在下面，
  别看这份文档里的数字。若某次改完解析器后这两个数长期为 0，说明公告措辞仍解析不出来（去看
  `dealhunter probe -source nc-vouchers-swj` 的详情正文），而不是提醒通道坏了。

```bash
ssh alienware-life 'sudo -u dealhunter /opt/deal-hunter/deal-hunter events -config /etc/deal-hunter/config.json -data /var/lib/deal-hunter'
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

## 次日确认项：2026-09-21 已复核（九条都有结论）

1. **恰好发了一份日报** —— VERIFIED。`kind=daily` 今天 2 行（飞书 + OpenClaw drop 各一次），
   `daily:last_sent = 2026-09-21T01:15:40Z` = 北京 09:15:40，落在 09:00–09:31（30m 采集间隔 + jitter）。
2. **`live_verified` 没上升 —— 但原因不是原来写的"预算不够"，那条假设被数据证伪**：
   今天新入库的 17 行里 `link_kind` 是 `third_party` 11 行、未判定 6 行，**official 0 行**。
   校验确实跑了（当日 `official lookups used=25`），是这些条目本身就是 linux.do / v2ex 的论坛帖，
   找不到"厂商官方入口"是正确答案，不是配额饥饿。以后判断这条要看**判定分布**而不是只看
   `live_verified` 这一个数；单看一个数会把"没找到"读成"没去找"。
3. **插队没有变成骚扰** —— 当天结论 VERIFIED，但当天**没有样本**，所以那只是"没坏"而不是"能用"。
   当时记录为"90 分门槛零命中"。（那句里的键名 `urgent.sent_today` 是手抄错的，真实键是
   `urgent:sent`，值形如 `{"date":…, "n":…}`；以 `sudo -u dealhunter` 读 `state.json` 为准。）
4. **90 分插队通道第一次被真实数据触发** —— VERIFIED（2026-09-21 14:19 北京）。`Qwen3.8-Flash
   限时免费一周`（`ai_free`，93 分）在采集轮启动后入库，日志两行 `notify: delivered kind=urgent
   items=1`（飞书 + OpenClaw drop 各一次），轮次行 `urgent_sent=1 urgent_held=0`，
   `urgent:sent` 记到 `n:1` = 当天上限 `max_per_day: 1` 用满，第二条高分会被 `held` 挡住。
   这条同时把 ROADMAP 里"理论 87–97"的那个估值钉成了一个真实观测值：**93 落在带内**。
   与当天的日报不冲突：`kind=daily` 在 09:15 北京发出（早于该条目入库），读者当天收到的是
   一份日报 + 一条插队，正是约定的两种消息。
   仍未验证的是**一天内第二条高分被 held** 那一半 —— 需要当天有两条 ≥90 分，目前没有。
5. **`reparse` 把解析修复追到了旧行上** —— VERIFIED（同日稍后，生产 `4905a74`）。干跑报 14 行可补，
   `-write` 后重跑报"无需改动"（幂等），带时刻的行 4 → 18，`events` 跟踪 2 → 8 条。
   **一处我说过头的话要更正**：我原以为"5 条已结束的促销正在污染今天的日报"，`daily -dry` 显示它们
   只有 58–68 分，今天本就排在 15 条之外 —— 改变的是**资格**（它们再也不会顶进日报），不是今天的内容。
   真实可见的变化是另一头：89 分的 `9月30日前免费获取Qwen3.8-Flash` 现在带着"截止 09-30"进日报，
   并进入到期提醒的跟踪列表。
   落盘前手动跑了一次 `deploy/backup.sh`（436K / 1085 行位 / 自解校验通过），旧行仍在 JSONL 历史里
   （追加式），所以这一步随时可回退。
6. **补完两条时间读法后再跑一次 `reparse`** —— VERIFIED（`881717b` 部署后）。干跑报 2 行：
   93 分那条 `Qwen3.8-Flash 限时免费一周` 拿到 `2026-09-27 23:59`（原文写的是「9月20日至27日」，
   逐字核过），DeepSeek 那条按"为期两周"拿到 10-04。写回后幂等重跑报"无需改动"，
   `events` 跟踪 8 → 10 条，日报第一行从"没有截止"变成带 `截止 09-27`。
   带时刻的行：743 条去重记录里 17 条有截止、3 条有开抢。
   **下一步的自然复核点**：09-27 北京 20:59 之后那条应当在 `kind=event` 日志里出现且只出现一次
   （两个通道各一行），次日日报里它应当已经不在（截止已过）。这是 M3 挂起项第一次有真实的
   近期窗口，判据命令就在上面。
7. **排程压缩在频繁部署的机器上从来没跑到过** —— VERIFIED（`doctor` 亲口报的）。
   `! compaction 从未压缩过`：触发条件是"连续 48 轮"这个**进程内**计数，30 分钟一轮要连续 24 小时
   不重启才凑得满，今晚六次部署各自把它清零，所以 `maint:last_compact` 这个键在生产上根本不存在。
   当晚手动跑了一次 `compact` 验证新写的清理路径（先备份：448K / 1115 行自解校验通过）：
   `deals.jsonl` 1115 → 749 行而条目数 `749 → 749`（只折叠重写，一行没丢），`state.json` 95 → 94 键
   （清掉的是退役的 `digest:last_sent`；64 个 `official:deal:*` 全部因为所属行还在而保留）。
   压缩后 `events` 仍 10 条、`daily -dry` 仍 15 条且第一条带 `截止 09-27`，最近 15 分钟日志 0 个 ERROR。
   **是否给它配一条 systemd timer 是运维决定**——它会删数据，本项目不擅自启用。
   （2026-09-22 更新：这条已经不需要外部定时器了。排程改成每轮检查、以盘上记号节流，
   见 ROADMAP Known Risks 同条与下面 M19 那两行。）
8. **日报口径收窄掉"向人求助"的帖子** —— VERIFIED（`7b475ee` 部署后 `daily -dry` 对比）。
   改前 15 条里有 5 条读者领不到（85 分"不懂，求佬解答"、83 分"请教推荐哪个plan套餐"、
   "帮我看看…是什么意思"、"求推荐网站的模板"、"精装修交付装修请教"），改后这 5 条全部消失、
   真实条目补位，仍是 15 条。中文子串误伤是在语料上量出来的，不是想到的：裸 `请教` 会打掉
   "免费申**请教**程"两条（学生许可证、魔搭额度申请），它们现在钉在测试的接受表里。
   补位后暴露的另一件事（同功能两条占 1 槽）与它的量数据写在 ROADMAP「已实测否决的方向」，
   **结论是不做标题前缀折叠**：当天高分前 40 名里另一对长前缀是"公告 vs 提问帖"，误伤确定、收益 1 槽。
9. **备份与压缩都有了"上次成功是多久前"的读数** —— VERIFIED（生产 `dc977a4`）。
   `backup.sh` 成功后在状态目录留 `backup.stamp`（实测 `0640 dealhunter:dealhunter`，内容只有
   UTC 时刻与归档名），doctor 两行现在都绿：`✓ compaction 上次压缩 0分钟前`、
   `✓ backup 上次成功备份 53分钟前 · deal-hunter-20260921-135711.tar.gz`。
   做这件事的过程中揪出两个谎：**手动 `compact` 原先不写 `maint:last_compact`**（我 08:31 压缩过，
   doctor 却报"从未压缩过"，排程器下次还要白跑一遍）；以及**跑 doctor 的测试漏写 `-net=false`** ——
   `-net` 默认 true，每次调用都真实探测 15 个信源，单跑 106 秒（`-net=false` 只要 1.8 秒），
   我新加的三条测试因此在门禁里挂满 600 秒超时。全部转离线后 cmd 包回到 0.15 秒，
   race 检测也才真正覆盖得到这几条。

## Current Version

生产二进制戳 `f12bd13`，最后一次改运行时代码的提交是 `525465f` —— 两者不等，而生产**不落后**。
所以判据不是"相等"，是**祖先关系**：

```bash
code=$(git log -1 --format=%H -- '*.go' ':!*_test.go' go.mod)   # 只算进得了二进制的改动
stamp=$(ssh alienware-life "sudo -u dealhunter /opt/deal-hunter/deal-hunter version" | sed -n 's/.*commit=\([0-9a-f]*\).*/\1/p')
git merge-base --is-ancestor "$code" "$stamp" && echo "生产不落后" || echo "生产缺代码改动"
```

`--is-ancestor` 为真 = 戳里已经含了全部运行时代码，多出来的那些提交只改文档；为假才是真落后，
此时 `git log --oneline $code ^$stamp` 列出缺的是什么。写成相等会在每个"只改文档就重新打包"的
提交上误报落后（`f12bd13` 就是这样：它相对 `525465f` 只多一份文档）。
两个坑都踩过：`'_test.go'` 不排除时，`1bc9614` 只改评分测试就被判成"生产落后"，而它没碰运行时代码；
`':!docs'` 这种排除法又会把 `deploy/` 的单元文件算进"代码"，在只改部署清单的提交上给假阴性。

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

- **手动跑 CLI 必须带 `-data /var/lib/deal-hunter`**（2026-09-22 我自己踩的）：数据目录来自
  `DH_DATA_DIR`，而它写在 `/etc/deal-hunter/deal-hunter.env` 里 —— `sudo -u dealhunter` 不加载
  那个文件，systemd 才加载。漏掉 `-data` 时 CLI 退回相对路径 `./data`，在 `/home/life` 下就是
  `mkdir /home/life/data: permission denied`，`doctor` 于是报 `✗ data_dir / ✗ store` 三项失败并
  `exit=1` —— 看着像生产坏了，其实探测根本没碰到状态目录。判据：`doctor` 那三行必须是 `✓` 且
  `compaction` 有"上次压缩 X 前"，才算它真读到了库。

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
- 读者的钟只有顶层 `timezone` 一处（`notify.feishu.timezone` 字段已在 M8 删除；旧配置里留着它
  也只是惰性废弃键，不影响运行）。写错时区在 `config.Validate()` 就拒绝，不再静默退回主机钟。
- OpenClaw 的开关只有 JSON 里的 `notify.openclaw.enabled`，**没有** `DH_OPENCLAW_ENABLED` 环境变量。

