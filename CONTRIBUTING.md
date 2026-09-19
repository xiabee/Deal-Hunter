# 🤝 参与贡献

感谢你想让这台羊毛雷达更锋利。花两分钟看完本文，可以让我们都少返工。

## 一句话规则

> **零外部依赖** + **离线可测** + **仓库里永远没有密钥和内网主机名**。

## 环境

- Go ≥ 1.24（`go.mod` 只要求语言版本，无需任何第三方模块）
- 可选：PowerShell（Windows 门禁）或 bash（Linux/macOS 门禁）

```bash
make help      # 所有可用目标
make ci-quick  # 快速门禁
make test      # 只跑测试
```

## 目录约定

```
cmd/dealhunter/        CLI 入口与子命令
internal/model/        归一化数据结构（唯一事实来源）
internal/keywords/     中外文价格词表 · 厂商词典 · 到期解析
internal/scoring/      0-100 置信分与可解释理由
internal/sources/      采集器：每种 kind 一个文件，共享 base 辅助
internal/pipeline/     采集 → 去重 → 评分 → 投递 的一轮编排
internal/notify/       投递后端：飞书卡片 / OpenClaw / 文件 / 控制台
internal/store/        JSONL 历史 + 状态游标
internal/httpapi/      只读接口与内置面板（go:embed）
internal/secretlint/   仓库敏感信息与内网拓扑扫描（CI 门禁）
deploy/                systemd 单元、env 模板、安装脚本、OpenClaw 集成
scripts/               ci-local.{sh,ps1} 与 ci-office.sh
```

## 新增一个采集器

1. 在 `internal/sources/` 新建 `<kind>.go`，内嵌 `base` 并实现 `Fetch(ctx)`；
   能复用的解析请放进 `htmlSegments` / `cleanText` / `resolveURL` 等已有辅助。
2. 在 `internal/config/config.go` 注册 kind 常量并加入 `Validate()` 白名单。
3. **补一个离线 fixture 测试**：`internal/sources/testdata/` 放样本，
   用 `stubFetcher` 断言标题、链接归一化、时间解析、关键词闸门与限制条数。
   CI 不允许需要联网的测试。
4. 若是新默认源，确认它在部署网络下真的可达（`dealhunter probe -source <name>`），
   并按「自主探测 > 官方页 > 聚合」的顺序给 `trust`。

## 新增一个投递后端

实现 `notify.Notifier`（`Name()` + `Send()`），在 `pipeline.New` 里按配置装配，
并保证：任一后端失败不影响其它后端、失败条目仍能进入日报补投。
外部命令一律用固定 argv 的 `exec.CommandContext`，**不要**经过 shell。

## 提交与 PR

- 提交前：`git config core.hooksPath .githooks`（可选）或手动 `make ci-quick`
- _commit message_：`scope: 一句话说清做了什么`（例如 `sources: 增加模型目录价格翻转检测`）
- PR 里请附：改动动机、跑过的门禁命令、**实际抓到的样例输出**（脱敏后）
- 没有 GitHub Actions 可用：请在 PR 中贴出 `bash scripts/ci-local.sh` 与
  `DH_CI_HOST=<你的构建机> bash scripts/ci-office.sh` 的通过结果

## 红线 🚫

- ❌ 任何真实 webhook / token / 聊天 ID / 内网 IP / 主机名 / `.ts.net` 域名
- ❌ 把密钥写进 JSON 配置或测试字面量。**夹具请用合成值**（`.invalid` 域名、
  `100.96.24.7` 这类假地址、`__PASTE_HERE__` 占位符），确需保留示例值时用 <!-- secretlint:ignore synthetic documentation example, not a real host -->
  `secretlint:ignore` + 理由，写在同一行
- ❌ 用 `"abc" + "def"` 之类的拼接绕过扫描器：门禁会先把字面量拼回去再匹配，
  而且真实值拆开也还是真实值——曾经就是这样把运维者的 Tailscale 地址留在了公开仓库里
- ❌ 为了「测试通过」而放宽 `secretlint` 规则或跳过 `-race`
- ❌ 引入第三方模块（确有必要时，请在 issue 里先讨论理由）
- ❌ 无节流地抓取：请保持每 host 串行限频与合理轮询间隔

## 许可

提交即表示你同意你的贡献以 [MIT](LICENSE) 授权。
