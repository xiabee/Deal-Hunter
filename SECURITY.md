# 🔐 Security Policy

Deal-Hunter 是一个会**主动访问外部站点**并把结果**推送到你的聊天工具**的常驻服务，
因此安全边界是设计的一部分，而不是事后补丁。

## 支持的版本

| 版本 | 支持 |
|---|---|
| `main` (HEAD) | ✅ |
| 其它 | 社区 PR 直接评审 |

## 报告漏洞

请勿开公开 issue。通过仓库的 *Settings → Report a vulnerability*（GitHub 私密安全报告）提交，
或直接联系维护者。我们会在 72 小时内确认，并在修复发布后披露细节。

## 内建防护（都有对应测试）

| 风险 | 处理 | 测试 |
|---|---|---|
| 凭证泄漏进仓库 | 密钥只从 `DH_*` 环境变量读取；结构体字段是 `json:"-"`；CI 扫描全仓 | `TestSecretsCannotBeSmuggledThroughTheConfigFile`, `TestRepositoryIsOpenSourceClean` |
| 凭证出现在日志/接口/错误里 | `internal/redact` 对 webhook token、`key: value`、query 参数脱敏；上游错误先脱敏再入库 | `TestNoCredentialEverLeavesTheProcess`, `Test*redact*` |
| 只读面板被暴露到公网 | `config.Validate()` 只允许回环 / `100.64/10` / `.ts.net` 绑定，公网需 `DH_ALLOW_PUBLIC_BIND=1` | `TestPublicBindIsRefused`, `TestLoopbackAndTailscaleBindsAreAllowed` |
| 被恶意信息源诱导打内网（SSRF） | 出站解析后拦截回环/RFC1918/链路本地/云元数据；元数据地址即使放开内网也不放行 | `TestBlockedTargets`, `TestPrivateTargetsAllowedWhenConfigured` |
| 摘要接口路径穿越 | 固定文件名 + `filepath.Abs` 前缀校验，越界返回 403 | `TestDigestServesMarkdownForOpenClaw` |
| 采集面把消息文本变成命令 | OpenClaw 中转使用**固定 argv**（`exec.Command`，无 shell）；argv 单元素承载正文 | `TestRelayNeutralisesShellMetacharacters` |
| 写接口 / CSRF 面 | 全部路由只允许 `GET`；其余 405；`X-Frame-Options: DENY`、CSP `default-src 'self'`、`no-store` | `TestSecurityHeadersAndReadOnlySurface` |
| 单源故障拖垮整体 | 每源独立超时 + panic  recover；投递失败只影响该后端 | `TestSourceFailureIsIsolated`, `TestFanOutToleratesOneFailingBackend` |
| systemd 权限过大 | `NoNewPrivileges`、`ProtectSystem=strict`、`PrivateTmp`、`RestrictAddressFamilies`、空 `CapabilityBoundingSet` | 部署时 `systemd-analyze security deal-hunter` |
| 误推无关/已过期内容 | 噪音词与过期截止时间直接丢弃 | `TestNoiseAndExpiryAreNeverStored` |
| 把仿冒/钓鱼域名当成官方链接 | `internal/official` 只认人工精选的厂商域名表，子域必须落在白名单域内；`moonshot.cn.evil.test`、`evil-bigmodel.cn` 一律拒绝；改写前必须实测 2xx/3xx，**且跳转后的域名仍属该厂商** | `TestVendorForDomainPrefersLongestMatch`, `TestSearchOnlyAcceptsVendorOwnedHosts`, `TestRedirectOffAllowlistIsRejected`, `TestCanonicalForFollowsOfferKind`（校验每个精选页确属该厂商） |
| 把厂商入口页冒充成活动证据 | 精选入口页只作为 `meta.vendor_url` 侧链（「去官网核实」），既不替换主链接也不加分；只有用这条羊毛自己的关键词在厂商站内搜到并校验的页面才算定位到活动 | `TestEntryPageIsASideDoorNotAClaim`, `TestCardOffersVendorSiteWithoutClaimingIt`, `TestDeadEntryPageIsNotOffered` |
| 把第三方转述伪装成官方公告 | 找不到厂商自有域名上的页面时保留原链接并标注 `third_party`，卡片与面板如实说明成色 | `TestCardKeepsUnverifiedLinkHonest`, `TestMissingDepsDegradeWithoutRewriting`, `TestUnknownVendorIsLeftAlone` |
| 链接改写环节被用来打内网 | 只探测白名单厂商域名，出站仍走 `httpx` 的 SSRF 闸门；每轮探测次数受 `max_official_lookups` 限制 | `TestBlockedTargets`, `TestBudgetCapsLookups`, `TestVerdictIsReusedNextRound` |

## 敏感信息扫描器

`internal/secretlint` 是仓库的发布门禁，覆盖 24 类凭证特征（飞书/Lark 群机器人 webhook、
GitHub、Tailscale `tskey-`、AWS、GCP、Slack、Stripe、SendGrid、Hugging Face、GitLab、
Telegram、Server酱、钉钉、JWT、DSN、URL 内嵌账号密码、`key: value` 赋值、私钥块）、
高熵字符串，以及**内网拓扑**（CGNAT 地址、`.ts.net` 主机名、RFC1918 地址）。

扫描时会把**拼接的字面量先拼回去**再匹配：`"100.96.24." + "7"`、`"AKIA" + "<16位>"` <!-- secretlint:ignore synthetic documentation example, not a real host -->
这类写法过去能躲过逐行匹配，现在不能。这条规则是被真实事故逼出来的——一个测试夹具
用拼接藏起了运维者自己的 Tailscale 地址，源码里看不见，仓库里却已经是明文。

```bash
go run ./cmd/dealhunter secretscan -C .     # 手工扫描
bash scripts/ci-local.sh                    # CI 会强制执行
```

**示例值一律用合成数据**，不要拿真实地址/真实 ID 改改就当夹具：
`100.96.24.7`、`router.example.ts.net`、`.invalid` 域名、`__PASTE_HERE__` 占位符。 <!-- secretlint:ignore synthetic documentation example, not a real host -->
确需保留时在**该行末尾**注明原因（隐藏式绕过不接受）：

```go
"100.100.100.100:8765", // secretlint:ignore CGNAT fixture for the /10 range check
```

## 部署侧建议

1. 面板保持 `127.0.0.1`；需要跨机器访问时改用 Tailscale 地址，**不要**开放公网端口。
2. `/etc/deal-hunter/deal-hunter.env` 权限 `0600`，属组为服务账户；不要提交、不要贴进 issue。
3. 若启用 OpenClaw 中转，使用 `deploy/sudoers-deal-hunter` 的最小授权，**不要**给服务账户读 OpenClaw 配置的权限。
4. 飞书群机器人请勾选「签名校验」，只加到目标群，不要把 webhook 当公开链接分享。
5. 增加信息源时优先 `https`；`kind: search` 请保持逐轮轮换、不要调高频率去打击搜索引擎。
