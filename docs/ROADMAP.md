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
| M7（备份与恢复） | `deploy/backup.sh`：打包状态与配置后**自解一次做对比**（行数 / config 在位 / sha256），`umask 077` 因为归档里含 env 密钥；`deal-hunter-backup.{service,timer}` 每晚 04:30 且 `Persistent`；恢复路径写进 README 并在生产演练过 | 本次提交 |

| M8（去重与去缝） | 读者时钟只剩顶层 `timezone` 一处，且写错在启动时就拒绝；`notify.DealsOf` 这个只为测试存在的接缝删掉（`m.Deals` 本来就是导出字段） | 本次提交 |

1. **限时事件的另一半：区县与省级栏目按「信源准入」流程实测后再加**（M3 已给出做法与判据）。
   2026-09-21 按市政府站内可发现的栏目量过一轮：`bmdt`（部门动态）/ `jrnc`（今日南昌）/
   `gwyxx`（政务信息）三个列表页都 200 / 32KB / 15 行带日期，结构与已接入的 `tzgg` 相同
   （采集器不用改就能解析），但**都不含消费券样本**，`bmdt` 标题抽样全是"召开…推进会 /
   普法宣传活动 / 政府开放周"这类机构动态。按「必须见到含券样本才准入」这条规矩，它们今天
   **不合格**，接进来只会让日报变长。真正的消费券公告就落在已经接入的 `tzgg`（商务局 + 市政府），
   而市级页面里出现的"东湖 / 西湖 / 青山湖 / 红谷滩 / 南昌县 / 新建区 / 安义"是**公告标题里的区县名**，
   不是可跳转的区县站链接 —— 区县商务局页面不在这两台主机的链接图里，要靠猜 URL 才能拿到，
   本项目不猜。等有真实活动跑起来时再量一次即可。

## 已实测否决的方向（别再试第二遍）

- **「官方目录差分：推理云 / 向量库 / GPU 云」**（2026-09-21 从生产出口实测后否决）：
  OpenAI 兼容的 `/v1/models` 全部要密钥 —— `api.deepseek.com` `api.moonshot.cn` `api.groq.com`
  `api.together.xyz` `api.siliconflow.cn/v1/models` 一律 401，`api.siliconflow.cn/v1/model/list/all`
  404，`api.github.com/models` 404；只有 `openrouter.ai/api/v1/models` 是公开 JSON（200 / 738KB /
  446 个 id / 带 `prompt`、`completion` 每 token 价格），而它已经在采集。**公开目录这条路没有第二个入口。**
  定价页方向同样不通：`siliconflow.cn/pricing`、`bigmodel.cn/pricing`、`autodl.com/pricing`、
  `cloud.zilliz.com.cn/pricing`、`volcengine.com/product/vikingdb` 全部 200，但页面里
  **服务端渲染的价格标记 `¥<数字>` 命中数为 0**，其中三个只有 3–6 KB 的 JS 壳。要读它们得上无头浏览器，
  而本项目是零第三方依赖的单二进制、不跑浏览器，用户的夜间调度也明确要求不拉起可见窗口。
  结论：这类信息只能靠现有的 `search` 采集器从第三方转述里拿（已在跑），或者等官方给出无需鉴权的目录接口。

## Known Risks

- **投递扇出任一成功即算送达**：一天只有一份消息，所以某个通道坏了当天这份就只存在于日志里。
  目前靠 `notify: partially delivered` 警告与 `/api/v1/status` 暴露，不做重试。真需要更强保证时要先定策略（去重 vs 漏投）。
- **备份目前与生产同盘**：`/var/backups/deal-hunter` 在 `alienware-life` 自己的磁盘上，能救误删与
  状态损坏，救不了整机故障。跨机那份要设 `DH_BACKUP_PUSH`，目标机器由用户定（见 STATUS Notes）。
- **日报只在采集轮结束后检查**：`daily.at` 到点后最多再等一个 `interval`（默认 30m）才发；发送失败下一轮重试。
- **插队门槛 90 分是估值**：「模型刚刚转免费」这类事件实测是否稳定 ≥90 未在真实采集里验证过（评分是加法的，理论 87–97）。
- **`state.json` 里会残留 `digest:last_sent`**：盘点通道已删，旧键没人读也没人清理；`Compact` 只重写 `deals.jsonl`。
- **升级后旧配置里的废弃键不报错也不生效**（`notify.digest`、`notify.feishu.{min_score,max_per_run,silent_hours}`、`filter.min_score`），`install.sh` 会打一行提示。

## Technical Debt
- `internal/scheduler` 没有测试文件：`Loop` 直接依赖 `*pipeline.App`，要测触发逻辑得先抽一个小接口（**故意没做**，避免为测试造抽象）。
- 采集器类型有 7 种，`sources.New` 是一个 switch；再加两类以上时值得回到注册表写法。

## Non-Goals

- 不引入数据库、Docker、消息队列、微服务拆分或前端构建链（JSONL + 单二进制是特性不是债）。
- 不跑无头浏览器去读 SPA 定价页：那等于把"零第三方依赖"换成一个 Chrome。这类信息拿不到就是拿不到，
  宁缺毋滥（实测见「已实测否决的方向」）。
- 不用 GitHub Actions 当门禁（额度优先留给代码托管与 Release）。
- 不做多群路由 / 分类订阅：目前只有一个读者。
- 不把 `0.0.0.0` 暴露成公网服务，也不为公网访问写认证层。
