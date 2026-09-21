// Command dealhunter is the Deal-Hunter single binary: a collector daemon, a
// read-only status API and the operational subcommands used by CI and deploys.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/httpapi"
	"github.com/xiabee/deal-hunter/internal/httpx"
	"github.com/xiabee/deal-hunter/internal/keywords"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/notify"
	"github.com/xiabee/deal-hunter/internal/pipeline"
	"github.com/xiabee/deal-hunter/internal/scheduler"
	"github.com/xiabee/deal-hunter/internal/secretlint"
	"github.com/xiabee/deal-hunter/internal/sources"
	"github.com/xiabee/deal-hunter/internal/store"
	"github.com/xiabee/deal-hunter/internal/version"
)

const usage = `Deal-Hunter — 全网羊毛雷达（每天一份日报）

用法：
  dealhunter [全局参数] <命令> [命令参数]

命令：
  run           常驻运行：定时采集 + 每天一份日报 + 只读状态服务
  once          立刻跑一轮后退出（-serve 可同时拉起 API）
  serve         只提供只读状态 API，不采集
  probe         采集单个信息源并打印结果（不写库、不推送）
  sources       列出全部信息源
  deals         打印最近入库的发现
  daily         立刻发送今天的日报（占用当天那一份）；-dry 只列出不发送
  events        列出正在跟踪的限时事件（开抢与截止两端、各自的状态）
  notify-test   向所有已配置通道发送自检消息
  doctor        配置、密钥、目录、外连可达性体检
  secretscan    扫描仓库中的凭证与内网拓扑信息（开源发布门禁）
  compact       压缩历史库
  reparse       用当前解析器重读已入库行的开抢/截止时刻（默认只报告，-write 落盘）
  version       打印版本

全局参数：
  -config FILE  配置文件（默认 $DH_CONFIG 或 ./config/deal-hunter.json）
  -data DIR     数据目录（覆盖 DH_DATA_DIR）
  -log LEVEL    debug|info|warn|error
  -json         日志使用 JSON 格式（适合 journald 采集）
`

func main() { os.Exit(cli(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

func cli(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	globals := flag.NewFlagSet("global", flag.ContinueOnError)
	globals.SetOutput(stderr)
	configPath := globals.String("config", "", "配置文件路径")
	dataDir := globals.String("data", "", "数据目录")
	logLevel := globals.String("log", "", "日志级别")
	logJSON := globals.Bool("json", false, "JSON 日志")
	_ = globals.Parse(normalizeGlobals(args))

	rest := globals.Args()
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		fmt.Fprint(stdout, usage)
		if len(rest) == 0 {
			return 2
		}
		return 0
	}
	cmd, cmdArgs := rest[0], rest[1:]

	cfg, err := loadConfig(*configPath, *dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误：%v\n", err)
		return 2
	}
	level := cfg.LogLevel
	if *logLevel != "" {
		level = *logLevel
	}
	log := newLogger(level, *logJSON, stdout)

	dispatch := map[string]func(context.Context, []string) int{
		"run":         func(c context.Context, a []string) int { return cmdRun(c, cfg, log, stdout, a) },
		"once":        func(c context.Context, a []string) int { return cmdOnce(c, cfg, log, stdout, a) },
		"serve":       func(c context.Context, a []string) int { return cmdServe(c, cfg, log, stdout, a) },
		"probe":       func(c context.Context, a []string) int { return cmdProbe(c, cfg, log, stdout, a) },
		"sources":     func(_ context.Context, _ []string) int { return cmdSources(cfg, stdout) },
		"deals":       func(_ context.Context, a []string) int { return cmdDeals(cfg, stdout, a) },
		"daily":       func(c context.Context, a []string) int { return cmdDaily(c, cfg, log, stdout, a) },
		"events":      func(_ context.Context, _ []string) int { return cmdEvents(cfg, log, stdout) },
		"notify-test": func(c context.Context, _ []string) int { return cmdNotifyTest(c, cfg, log, stdout) },
		"doctor":      func(c context.Context, a []string) int { return cmdDoctor(c, cfg, log, stdout, a) },
		"secretscan":  func(_ context.Context, a []string) int { return cmdSecretScan(stdout, stderr, a) },
		"compact":     func(_ context.Context, a []string) int { return cmdCompact(cfg, log, stdout, a) },
		"reparse":     func(_ context.Context, a []string) int { return cmdReparse(cfg, stdout, a) },
		"version":     func(_ context.Context, _ []string) int { fmt.Fprintln(stdout, version.String()); return 0 },
	}
	fn, ok := dispatch[cmd]
	if !ok {
		fmt.Fprintf(stderr, "未知命令 %q\n\n%s", cmd, usage)
		return 2
	}
	return fn(withSignals(ctx), cmdArgs)
}

// normalizeGlobals hoists global flags that appear after the subcommand.
func normalizeGlobals(args []string) []string {
	globalNames := map[string]bool{"-config": true, "--config": true, "-data": true, "--data": true,
		"-log": true, "--log": true, "-json": true, "--json": true}
	var globals, others []string
	sawCmd := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, isFlag := strings.Cut(a, "=")
		if isFlag && globalNames[name] {
			globals = append(globals, a)
			continue
		}
		if !sawCmd && !strings.HasPrefix(a, "-") {
			sawCmd = true
		}
		if globalNames[a] && i+1 < len(args) {
			globals = append(globals, a, args[i+1])
			i++
			continue
		}
		others = append(others, a)
	}
	return append(globals, others...)
}

func withSignals(ctx context.Context) context.Context {
	c, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	_ = stop // cancelled when the process exits
	return c
}

func loadConfig(path, dataOverride string) (*config.Config, error) {
	if path == "" {
		path = os.Getenv(config.EnvConfig)
	}
	if path == "" {
		if _, err := os.Stat("config/deal-hunter.json"); err == nil {
			path = "config/deal-hunter.json"
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if dataOverride != "" {
		cfg.DataDir = dataOverride
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func newLogger(level string, jsonOut bool, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch strings.ToLower(level) {
	case "debug":
		opts.Level = slog.LevelDebug
	case "warn", "warning":
		opts.Level = slog.LevelWarn
	case "error":
		opts.Level = slog.LevelError
	}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if jsonOut {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

func cmdRun(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stdout)
	skipFirst := fs.Bool("no-first", false, "启动时不立刻执行第一轮")
	_ = fs.Parse(args)

	app, err := pipeline.New(cfg, log)
	if err != nil {
		fmt.Fprintf(stdout, "启动失败：%v\n", err)
		return 1
	}
	defer app.Close()
	log.Info("deal-hunter starting", "version", version.Version, "data", cfg.DataDir,
		"interval", cfg.Interval.String(), "sources", len(cfg.EnabledSources()), "backends", app.Backends())

	api := httpapi.New(cfg, app, log)
	errCh := make(chan error, 1)
	go func() { errCh <- api.Serve(ctx) }()

	loop := &scheduler.Loop{App: app, Cfg: cfg, Log: log, SkipFirst: *skipFirst}
	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(ctx) }()

	select {
	case err := <-runErr:
		if err != nil && ctx.Err() == nil {
			log.Error("scheduler stopped", "err", err)
			return 1
		}
	case err := <-errCh:
		if err != nil {
			log.Error("api stopped", "err", err)
			return 1
		}
	}
	<-time.After(50 * time.Millisecond)
	log.Info("shutdown complete")
	return 0
}

func cmdOnce(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	fs.SetOutput(stdout)
	serve := fs.Bool("serve", false, "采集后保持 API 常驻")
	hold := fs.Duration("hold", 0, "配合 -serve 保持运行的时长，0 表示直到收到信号")
	_ = fs.Parse(args)

	app, err := pipeline.New(cfg, log)
	if err != nil {
		fmt.Fprintf(stdout, "启动失败：%v\n", err)
		return 1
	}
	defer app.Close()

	run, _ := app.RunOnce(ctx, "once")
	printRun(stdout, run)

	if !*serve {
		return 0
	}
	api := httpapi.New(cfg, app, log)
	log.Info("serving read-only API", "addr", api.Addr())
	if *hold > 0 {
		c, cancel := context.WithTimeout(ctx, *hold)
		defer cancel()
		ctx = c
	}
	return exitFromError(api.Serve(ctx))
}

func cmdServe(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer, args []string) int {
	app, err := pipeline.New(cfg, log)
	if err != nil {
		fmt.Fprintf(stdout, "启动失败：%v\n", err)
		return 1
	}
	defer app.Close()
	api := httpapi.New(cfg, app, log)
	log.Info("serving read-only API", "addr", api.Addr())
	return exitFromError(api.Serve(ctx))
}

func cmdProbe(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stdout)
	name := fs.String("source", "", "要探测的信息源名称（必填）")
	limit := fs.Int("n", 15, "最多打印条数")
	_ = fs.Parse(args)
	if *name == "" {
		fmt.Fprintln(stdout, "probe 需要 -source <name>；用 `sources` 查看可用名称")
		return 2
	}
	var found *config.Source
	for i := range cfg.Sources {
		if cfg.Sources[i].Name == *name {
			found = &cfg.Sources[i]
			break
		}
	}
	if found == nil {
		fmt.Fprintf(stdout, "未找到信息源 %q\n", *name)
		return 1
	}
	// Deliberately no store: probing must not advance production cursors.
	src, err := sources.New(sources.Deps{Cfg: *found, HTTP: newFetcher(cfg), Defaults: cfg.HTTP, Log: log, Now: time.Now})
	if err != nil {
		fmt.Fprintf(stdout, "构造信息源失败：%v\n", err)
		return 1
	}
	c, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	start := time.Now()
	deals, err := src.Fetch(c)
	if err != nil {
		fmt.Fprintf(stdout, "✗ %s 采集失败：%v\n", found.Name, err)
		return 1
	}
	dict := keywords.Default()
	fmt.Fprintf(stdout, "✓ %s (%s) 命中 %d 条，耗时 %s\n", found.Name, found.Kind, len(deals), time.Since(start).Round(time.Millisecond))
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "命中词\t分数\t标题\t链接")
	for i, d := range deals {
		if i >= *limit {
			break
		}
		annotate(d, dict)
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", strings.Join(kinds(d), ","), d.Score, truncate(d.Title, 46), truncate(d.URL, 52))
	}
	_ = w.Flush()
	return 0
}

// presentedLink mirrors what the card and the panel point at: the verified
// vendor page when we found one, otherwise the post the deal came from.
func presentedLink(d model.Deal) string {
	best, _, kind := notify.LinkFor(&d)
	if mark := notify.LinkMark(kind); mark != "" {
		return mark + " " + truncate(best, 46)
	}
	return truncate(best, 46)
}

func newFetcher(cfg *config.Config) *httpx.Client {
	return httpx.New(httpx.Options{
		UserAgent:    cfg.HTTP.UserAgent,
		Timeout:      cfg.HTTP.Timeout.D(),
		MaxBody:      int64(cfg.HTTP.MaxBodyKB) * 1024,
		PerHostMin:   time.Duration(cfg.HTTP.PerHostMili) * time.Millisecond,
		Retries:      cfg.HTTP.Retries,
		AllowPrivate: cfg.HTTP.AllowPrivateHosts,
	})
}

// annotate mirrors the pipeline's scoring so probe output matches production.
func annotate(d *model.Deal, dict *keywords.Dict) {
	d.EnsureFingerprint()
	text := d.TextBlob()
	d.Offers = dict.Scan(text)
	if res := dict.DiscountPct(text); res > 0 {
		d.DiscountPct = res
	}
	d.Vendors = dict.Vendors(text)
	for _, o := range d.Offers {
		if o.Kind == model.KindFree {
			d.IsFree = true
		}
	}
	d.Score = 40 + len(d.Offers)*10 + 10*boolInt(len(d.Vendors) > 0) + boolInt(d.IsFree)*20
	if d.Score > 100 {
		d.Score = 100
	}
}

func kinds(d *model.Deal) []string {
	var out []string
	for _, o := range d.Offers {
		out = append(out, string(o.Kind))
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func cmdSources(cfg *config.Config, stdout io.Writer) int {
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "状态\t名称\t类型\t可信度\t官方源\tURL")
	for _, s := range cfg.Sources {
		state := "on "
		if s.Disabled {
			state = "off"
		}
		sites := "-"
		if len(s.Sites) > 0 {
			sites = strings.Join(s.Sites, ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", state, s.Name, s.Kind, s.Trust, sites, s.URL)
	}
	_ = w.Flush()
	fmt.Fprintf(stdout, "\n共 %d 个信息源，启用 %d 个\n", len(cfg.Sources), len(cfg.EnabledSources()))
	return 0
}

func cmdDeals(cfg *config.Config, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("deals", flag.ContinueOnError)
	fs.SetOutput(stdout)
	n := fs.Int("n", 25, "条数")
	minScore := fs.Int("min", 0, "最低分数")
	_ = fs.Parse(args)

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stdout, "打开数据目录失败：%v\n", err)
		return 1
	}
	defer st.Close()
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "分数\t推送\t来源\t标题\t链接\t发现时间")
	shown := 0
	for _, d := range st.Recent(5000) {
		if d.Score < *minScore {
			continue
		}
		pushed := ""
		if d.Meta["pushed"] == "true" {
			pushed = "📣"
		}
		if d.Meta["dup_of"] != "" {
			pushed += "↻" // a repost of something already reported
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", d.Score, pushed, truncate(d.Source, 18),
			truncate(d.Title, 52), presentedLink(d),
			d.DiscoveredAt.Local().Format("01-02 15:04"))
		if shown++; shown >= *n {
			break
		}
	}
	_ = w.Flush()
	if shown == 0 {
		fmt.Fprintln(stdout, "暂无记录：先跑 `dealhunter once`")
	}
	return 0
}

// cmdDaily sends the day's briefing, or lists it with -dry. Sending one here
// consumes today's slot: the schedule will not send a second one until the next
// one arrives.
func cmdDaily(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("daily", flag.ContinueOnError)
	fs.SetOutput(stdout)
	dry := fs.Bool("dry", false, "只列出当前在效的羊毛，不发送")
	_ = fs.Parse(args)

	app, err := pipeline.New(cfg, log)
	if err != nil {
		fmt.Fprintf(stdout, "启动失败：%v\n", err)
		return 1
	}
	defer app.Close()

	now := time.Now()
	if *dry {
		live := app.DailyPreview(now)
		info := app.Daily(now)
		fmt.Fprintf(stdout, "当前在效 %d 条 · 日报时间 %s（%s）· 今日%s\n",
			len(live), info.At, cfg.Timezone, map[bool]string{true: "已发过", false: "还没发"}[info.SentToday])
		for _, d := range live {
			dl := d
			fmt.Fprintf(stdout, "  %s\n", notify.DailyLine(&dl, now))
		}
		if len(live) == 0 {
			fmt.Fprintln(stdout, "  （今天没有在效的羊毛，日报仍会发这一条空报）")
		}
		return 0
	}
	if err := app.SendDaily(ctx, now); err != nil {
		fmt.Fprintf(stdout, "日报发送失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "✓ 日报已发送，今天的这一份用掉了")
	return 0
}

func cmdNotifyTest(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer) int {
	app, err := pipeline.New(cfg, log)
	if err != nil {
		fmt.Fprintf(stdout, "启动失败：%v\n", err)
		return 1
	}
	defer app.Close()
	fmt.Fprintf(stdout, "通道：%s\n", strings.Join(app.Backends(), ", "))
	if err := app.NotifyTest(ctx); err != nil {
		fmt.Fprintf(stdout, "✗ 自检失败：%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "✓ 自检消息已送达所有通道")
	return 0
}

type check struct {
	Name   string
	Status string // ok | warn | fail
	Detail string
}

func cmdDoctor(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stdout)
	net := fs.Bool("net", true, "逐个信息源做外连探测")
	_ = fs.Parse(args)

	var checks []check
	add := func(name, status, detail string) {
		checks = append(checks, check{Name: name, Status: status, Detail: detail})
	}

	urgent := "关"
	if cfg.Notify.Urgent.Enabled {
		urgent = fmt.Sprintf("≥%d，每天最多 %d 条", cfg.Notify.Urgent.MinScore, cfg.Notify.Urgent.MaxPerDay)
	}
	remind := "关"
	if cfg.Notify.Event.Enabled {
		remind = fmt.Sprintf("开抢前 %v，到期前 %v，每天最多 %d 条",
			cfg.Notify.Event.Lead, cfg.Notify.Event.ExpiryLead, cfg.Notify.Event.MaxPerDay)
	}
	add("config", "ok", fmt.Sprintf("interval=%s 日报=%s（%s，地板 %d 分）突破=%s 提醒=%s",
		cfg.Interval, cfg.Notify.Daily.At, cfg.Timezone, cfg.Notify.Daily.MinScore, urgent, remind))
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		add("data_dir", "fail", err.Error())
	} else if probe := filepath.Join(cfg.DataDir, ".write-test"); os.WriteFile(probe, []byte("x"), 0o600) != nil {
		add("data_dir", "fail", "不可写："+probe)
	} else {
		_ = os.Remove(probe)
		add("data_dir", "ok", cfg.DataDir)
	}

	bindNote := "回环地址，仅本机可访问"
	switch {
	case strings.HasPrefix(cfg.Server.Bind, "127.") || strings.Contains(cfg.Server.Bind, "localhost"):
	case strings.Contains(cfg.Server.Bind, ":"):
		bindNote = "非回环绑定，请确认仅 Tailscale 可达"
	}
	if !cfg.Server.Enabled {
		bindNote = "API 已关闭"
	}
	add("server_bind", "ok", cfg.Server.Bind+" · "+bindNote)

	if cfg.Notify.Feishu.WebhookURL == "" {
		add("feishu", "warn", "未设置 "+config.EnvFeishuWebhook+"，无法直接推送")
	} else {
		status := "ok"
		if !strings.HasPrefix(cfg.Notify.Feishu.WebhookURL, "https://") {
			status = "fail"
		}
		host := cfg.Notify.Feishu.WebhookURL
		if i := strings.Index(host[strings.Index(host, "//")+2:], "/"); i > 0 {
			host = host[:strings.Index(host, "//")+2+i]
		}
		secretState := "未启用签名"
		if cfg.Notify.Feishu.Secret != "" {
			secretState = "已启用签名"
		}
		add("feishu", status, host+" · "+secretState)
	}
	if cfg.Notify.OpenClaw.Enabled {
		dir := cfg.Notify.OpenClaw.SkillDir
		if dir == "" {
			dir = filepath.Join(cfg.DataDir, "outreach")
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			add("openclaw_drop", "fail", err.Error())
		} else {
			add("openclaw_drop", "ok", dir)
		}
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		add("store", "fail", err.Error())
	} else {
		stats := st.Stats()
		add("store", "ok", fmt.Sprintf("已见 %d · 已推送 %d · 游标 %d · %d KB", stats.DealsSeen, stats.PushedSeen, stats.StateKeys, stats.DirSizeKB))
		cStat, cDetail := compactionState(st)
		add("compaction", cStat, cDetail)
		bStat, bDetail := backupFreshness(cfg.DataDir)
		add("backup", bStat, bDetail)
		st.Close()
	}

	if *net {
		cli := newFetcher(cfg)
		okCount := 0
		for _, sc := range cfg.EnabledSources() {
			c, cancel := context.WithTimeout(ctx, 25*time.Second)
			resp, err := cli.Get(c, sc.URL, sc.Headers)
			cancel()
			switch {
			case err != nil:
				add("source:"+sc.Name, "warn", err.Error())
			case resp.Status >= 400:
				add("source:"+sc.Name, "warn", fmt.Sprintf("HTTP %d", resp.Status))
			default:
				okCount++
				add("source:"+sc.Name, "ok", fmt.Sprintf("HTTP %d · %d KB", resp.Status, len(resp.Body)/1024))
			}
		}
		add("egress", map[bool]string{true: "ok", false: "warn"}[okCount > 0],
			fmt.Sprintf("%d/%d 个信息源可达", okCount, len(cfg.EnabledSources())))
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "结果\t检查项\t详情")
	fails := 0
	sorted := append([]check(nil), checks...)
	sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i].Status) < rank(sorted[j].Status) })
	for _, c := range sorted {
		icon := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
		if c.Status == "fail" {
			fails++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", icon, c.Name, truncate(c.Detail, 110))
	}
	_ = w.Flush()
	if fails > 0 {
		fmt.Fprintf(stdout, "\n体检未通过：%d 项失败\n", fails)
		return 1
	}
	fmt.Fprintln(stdout, "\n体检通过")
	return 0
}

func rank(s string) int {
	return map[string]int{"fail": 0, "warn": 1, "ok": 2}[s]
}

func cmdSecretScan(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("secretscan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "扫描根目录")
	max := fs.Int("n", 40, "最多打印条数")
	_ = fs.Parse(args)

	findings, err := secretlint.Scan(secretlint.DefaultOptions(*dir))
	if err != nil {
		fmt.Fprintf(stderr, "扫描失败：%v\n", err)
		return 2
	}
	if len(findings) == 0 {
		fmt.Fprintf(stdout, "✓ secretscan: 未发现凭证或内网拓扑信息（%s）\n", *dir)
		return 0
	}
	fmt.Fprintf(stderr, "✗ secretscan: 发现 %d 处敏感内容：\n", len(findings))
	for i, f := range findings {
		if i >= *max {
			fmt.Fprintf(stderr, "  … 另有 %d 处\n", len(findings)-*max)
			break
		}
		fmt.Fprintf(stderr, "  %s  %s:%d  %s\n", f.Rule, f.Path, f.Line, f.Snippet)
	}
	fmt.Fprintln(stderr, "\n如为有意保留的示例值，请在该行末尾加 secretlint:ignore 并说明原因。")
	return 1
}

func cmdCompact(cfg *config.Config, log *slog.Logger, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(stdout)
	days := fs.Int("days", 60, "保留天数")
	_ = fs.Parse(args)
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stdout, "打开数据目录失败：%v\n", err)
		return 1
	}
	defer st.Close()
	before := st.Stats().DealsSeen
	if err := st.Compact(*days); err != nil {
		fmt.Fprintf(stdout, "压缩失败：%v\n", err)
		return 1
	}
	// Same marker the scheduled sweep writes: a manual compaction must count, or
	// doctor keeps warning and the scheduler repeats work that was just done.
	if err := st.PutState(store.StateLastCompact, time.Now().UTC().Format(time.RFC3339)); err != nil {
		fmt.Fprintf(stdout, "✓ 压缩完成：%d → %d 条（但记不下时间：%v）\n", before, st.Stats().DealsSeen, err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ 压缩完成：%d → %d 条\n", before, st.Stats().DealsSeen)
	return 0
}

func printRun(w io.Writer, run *pipeline.Run) {
	if run == nil {
		fmt.Fprintln(w, "本轮未执行")
		return
	}
	fmt.Fprintf(w, "\n本轮 %s：新增 %d · 入库 %d · 突破推送 %d · 转入日报 %d · 用时 %s\n",
		run.Trigger, run.NewDeals, run.Stored, run.Pushed, run.UrgentHeld,
		run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  信息源\t命中\t入库\t耗时\t状态")
	for _, s := range run.Sources {
		status := "ok"
		if s.Err != "" {
			status = truncate(s.Err, 60)
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%dms\t%s\n", s.Name, s.Found, s.Stored, s.Ms, status)
	}
	_ = tw.Flush()
	for _, e := range run.Errors {
		fmt.Fprintf(w, "  错误：%s\n", truncate(e, 140))
	}
}

func exitFromError(err error) int {
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// cmdEvents lists the dated events the reminder channel is tracking, so "在等哪几场、
// 提醒过没有" is answerable without reading state.json.
func cmdEvents(cfg *config.Config, log *slog.Logger, stdout io.Writer) int {
	app, err := pipeline.New(cfg, log)
	if err != nil {
		fmt.Fprintf(stdout, "启动失败：%v\n", err)
		return 1
	}
	defer app.Close()

	now := time.Now()
	rows := app.UpcomingEvents(now)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "没有在跟踪的限时事件：只有公告里写了明确日期与时刻的券/活动才会被跟踪，解析不出来就不提醒。")
		return 0
	}
	e := cfg.Notify.Event
	fmt.Fprintf(stdout, "跟踪 %d 项限时事件（提醒窗口：开抢前 %v 至开抢后 %v，到期前 %v，每天最多 %d 条，门槛 %d 分）\n",
		len(rows), e.Lead, e.LateGrace, e.ExpiryLead, e.MaxPerDay, e.MinScore)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  时刻\t事由\t距今\t状态\t分数\t标题")
	for _, it := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\n",
			it.Moment.In(locOf(cfg)).Format("01-02 15:04"), it.Due,
			humanDelta(it.Moment.Sub(now)), eventMark(it, e), it.Score, truncate(it.Title, 40))
	}
	_ = tw.Flush()
	return 0
}

func eventMark(it pipeline.EventItem, e config.Event) string {
	switch {
	case it.Reminded:
		return "已提醒"
	case it.InWindow:
		return "窗口内待提醒"
	case it.Score < e.MinScore:
		return "低于门槛"
	case it.Moment.Before(time.Now()):
		return "已过窗口"
	default:
		return "等待窗口"
	}
}

// locOf resolves the display zone for the same clock the briefing uses.
// cmdReparse re-reads the clock fields of rows already in the store.
//
// A source's cursor only moves forward, so an announcement that was mis-parsed when
// it arrived is never revisited by the next collection round: the parser fix that
// would have read it lands on rows that no longer exist. This applies the current
// parser to what we already hold. It changes time fields only - never the score -
// because re-ranking old rows on a new parser would let a months-old finding jump
// into tomorrow's briefing.
func cmdReparse(cfg *config.Config, stdout io.Writer, args []string) int {
	fs := flag.NewFlagSet("reparse", flag.ContinueOnError)
	fs.SetOutput(stdout)
	write := fs.Bool("write", false, "写回数据目录（默认只报告要改什么）")
	limit := fs.Int("limit", 0, "只看最近 N 行（0 = 全部）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stdout, "打开数据目录失败：%v\n", err)
		return 1
	}
	defer st.Close()

	dict := keywords.Default()
	loc := locOf(cfg)
	now := time.Now()
	changed := 0
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  标题\t改动")
	for _, d := range st.Recent(*limit) {
		if d.Meta["dup_of"] != "" {
			continue // 折叠掉的副本不进日报，也不进提醒
		}
		anchor := d.DiscoveredAt
		if !d.PublishedAt.IsZero() {
			anchor = d.PublishedAt
		}
		if anchor.IsZero() {
			anchor = now
		}
		anchor = anchor.In(loc)
		text := d.TextBlob()
		// An absent reading is not a correction: a row whose stored deadline this
		// parser can no longer read keeps it. Dropping it would put a finished
		// offer back into the briefing, which is worse than the stale value.
		var diffs []string
		meta := d.Meta
		if meta == nil {
			meta = map[string]string{}
		}
		if exp := dict.ExpiresAt(text, anchor); exp != nil {
			if v := exp.Format(time.RFC3339); meta["expires_at"] != v {
				diffs = append(diffs, "截止 "+humanMetaTime(meta["expires_at"], loc)+" → "+exp.In(loc).Format("2006-01-02 15:04"))
				meta["expires_at"] = v
			}
		}
		if starts := dict.StartsAt(text, anchor); starts != nil {
			if v := starts.Format(time.RFC3339); meta["starts_at"] != v {
				diffs = append(diffs, "开抢 "+humanMetaTime(meta["starts_at"], loc)+" → "+starts.In(loc).Format("2006-01-02 15:04"))
				meta["starts_at"] = v
			}
		}
		if len(diffs) == 0 {
			continue
		}
		changed++
		fmt.Fprintf(tw, "  %s\t%s\n", truncate(d.Title, 34), strings.Join(diffs, "；"))
		if !*write {
			continue
		}
		d.Meta = meta
		if err := st.Save(&d); err != nil {
			fmt.Fprintf(stdout, "写回失败：%v\n", err)
			return 1
		}
	}
	if changed == 0 {
		fmt.Fprintf(stdout, "已入库行的时刻与当前解析器一致，无需改动\n")
		return 0
	}
	_ = tw.Flush()
	if *write {
		fmt.Fprintf(stdout, "✓ 已写回 %d 行（追加，旧行仍在历史里）\n", changed)
	} else {
		fmt.Fprintf(stdout, "以上 %d 行可补出时刻；确认后再加 -write 落盘\n", changed)
	}
	return 0
}

// humanMetaTime renders a stored RFC3339 meta value for the change report.
func humanMetaTime(v string, loc *time.Location) string {
	if v == "" {
		return "（无）"
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return v
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

func locOf(cfg *config.Config) *time.Location {
	if l, err := time.LoadLocation(cfg.Timezone); err == nil {
		return l
	}
	return time.Local
}

// humanDelta renders a lead time like "3小时30分" without pretending precision.
// backupFreshness reports the last time deploy/backup.sh finished successfully.
// The backup runs on a timer whose failures only reach the journal, and the archive
// is the one recovery path that is not on the same disk as the data - so "we have
// backups" has to be answered from a record, not from the unit existing.
func backupFreshness(dataDir string) (string, string) {
	b, err := os.ReadFile(filepath.Join(dataDir, "backup.stamp"))
	if err != nil {
		return "warn", "数据目录里没有 backup.stamp：backup.sh 没成功跑过" +
			"（未排程→systemctl enable --now deal-hunter-backup.timer；跑了就失败→journalctl -u 该 unit）"
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "warn", "backup.stamp 是空的"
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", fields[0])
	if err != nil {
		return "warn", "backup.stamp 的首字段不是 UTC 时刻: " + truncate(fields[0], 24)
	}
	age := time.Since(t)
	archive := ""
	if len(fields) > 1 {
		archive = " · " + truncate(fields[1], 34)
	}
	if age > 36*time.Hour {
		return "warn", "上次成功备份已是 " + humanDelta(age) + "前，超过每晚一次的节奏" + archive
	}
	return "ok", "上次成功备份 " + humanDelta(age) + "前" + archive
}

// compactionState reports when the store was last compacted. The scheduled trigger is
// an in-process round counter (every 48 rounds, and no more than once every 7 days), so
// a host that gets redeployed daily resets it before it fires and never compacts at all
// - which is why this is its own check rather than a line inside "store".
func compactionState(st *store.Store) (string, string) {
	b, ok := st.GetState(store.StateLastCompact)
	if !ok {
		return "warn", "从未压缩过（排程是连续 48 轮且距上次 7 天，重启会清零）；手动：deal-hunter compact"
	}
	var s string
	if json.Unmarshal(b, &s) != nil {
		return "warn", "maint:last_compact 读不出"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "warn", "maint:last_compact 不是时刻: " + truncate(s, 24)
	}
	age := time.Since(t)
	if age > 8*24*time.Hour {
		return "warn", "上次压缩已是 " + humanDelta(age) + "前，超过 7 天的排程"
	}
	return "ok", "上次压缩 " + humanDelta(age) + "前"
}

func humanDelta(d time.Duration) string {
	switch {
	case d < 0:
		return fmt.Sprintf("%.0f小时前", (-d).Hours())
	case d < time.Hour:
		return fmt.Sprintf("%d分钟", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f小时", d.Hours())
	default:
		return fmt.Sprintf("%d天", int(d.Hours())/24)
	}
}
