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

## 敏感信息扫描器

`internal/secretlint` 是仓库的发布门禁，覆盖 24 类凭证特征（飞书/Lark 群机器人 webhook、
GitHub、Tailscale `tskey-`、AWS、GCP、Slack、Stripe、SendGrid、Hugging Face、GitLab、
Telegram、Server酱、钉钉、JWT、DSN、URL 内嵌账号密码、`key: value` 赋值、私钥块）、
高熵字符串，以及**内网拓扑**（CGNAT 地址、`.ts.net` 主机名、RFC1918 地址）。

```bash
go run ./cmd/dealhunter secretscan -C .     # 手工扫描
bash scripts/ci-local.sh                    # CI 会强制执行
```

确需保留示例值时，在**该行末尾**注明原因：

```go
"100.100.100.100:8765", // secretlint:ignore CGNAT fixture for the /10 range check
```

## 部署侧建议

1. 面板保持 `127.0.0.1`；需要跨机器访问时改用 Tailscale 地址，**不要**开放公网端口。
2. `/etc/deal-hunter/deal-hunter.env` 权限 `0600`，属组为服务账户；不要提交、不要贴进 issue。
3. 若启用 OpenClaw 中转，使用 `deploy/sudoers-deal-hunter` 的最小授权，**不要**给服务账户读 OpenClaw 配置的权限。
4. 飞书群机器人请勾选「签名校验」，只加到目标群，不要把 webhook 当公开链接分享。
5. 增加信息源时优先 `https`；`kind: search` 请保持逐轮轮换、不要调高频率去打击搜索引擎。
