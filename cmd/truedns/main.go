// Command truedns is the true-dns CLI: a local DNS proxy that restores the
// real IP addresses of polluted domains (GitHub ecosystem by default) via
// encrypted DoH upstreams and takes over the system DNS configuration.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"truedns/internal/api"
	"truedns/internal/config"
	"truedns/internal/core"
	"truedns/internal/paths"
	"truedns/internal/platform"
	"truedns/internal/version"
)

const usageText = `true-dns — 还原被污染域名的真实 IP, 并接管系统 DNS 解析

用法:
  truedns <command> [flags]

命令:
  run       接管系统 DNS 并启动代理 (Ctrl+C 退出时自动恢复; 日常使用推荐)
  serve     仅启动代理, 不修改系统 DNS
  setup     仅接管系统 DNS (警告: 若代理未运行, 系统将无法解析域名)
  restore   恢复接管前的系统 DNS 设置
  status    查看接管状态与代理运行状态
  flush     清空代理缓存 (经本地控制 API)
  logs      查看代理日志 (排查启动失败等)
  tray      启动 Windows 系统托盘 GUI (仅 Windows)
  config    管理配置: config init | config show | config path
  version   打印版本信息

全局 flags:
  --config <path>    配置文件路径 (默认: Windows %ProgramData%\truedns\config.toml,
                     Linux /etc/truedns/config.toml, 普通用户 ~/.config/truedns/)
  --log-level <lvl>  日志级别 debug|info|warn|error (默认 info)

run/setup 在 Windows 上需要管理员权限 (会自动触发 UAC 提权, --no-elevate 可关闭)。
`

type command struct {
	usage string
	run   func(fs *flag.FlagSet, args []string) error
}

var commands = map[string]command{
	"serve":   {usage: "serve [--config path] [--log-level lvl]", run: cmdServe},
	"run":     {usage: "run [--config path] [--keep] [--no-elevate]", run: cmdRun},
	"setup":   {usage: "setup [--config path] [--force] [--no-elevate]", run: cmdSetup},
	"restore": {usage: "restore [--no-elevate]", run: cmdRestore},
	"status":  {usage: "status [--config path]", run: cmdStatus},
	"flush":   {usage: "flush [--config path]", run: cmdFlush},
	"logs":    {usage: "logs [-n 50] [--log-file path]", run: cmdLogs},
	"tray":    {usage: "tray [--config path] [--log-level lvl]", run: cmdTray},
	"config":  {usage: "config <init|show|path> [--config path] [--force]", run: cmdConfig},
	"version": {usage: "version", run: cmdVersion},
}

func main() {
	// Panics and fatal errors are mirrored into the log file: the elevated
	// instance on Windows runs in a fresh console window that disappears on
	// exit, so stderr alone would leave no trace of what went wrong.
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic: %v\n%s", r, debug.Stack())
			fmt.Fprint(os.Stderr, msg)
			appendErrorLog(msg)
			os.Exit(2)
		}
	}()
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		appendErrorLog(err.Error())
		os.Exit(1)
	}
}

// appendErrorLog best-effort appends a fatal error to the default log file.
func appendErrorLog(msg string) {
	f, err := openLog(defaultLogPath())
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "time=%s level=ERROR msg=%q\n", time.Now().Format(time.RFC3339), msg)
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stdout, usageText)
		return nil
	}
	name := args[0]
	if name == "help" || name == "-h" || name == "--help" {
		fmt.Fprint(os.Stdout, usageText)
		return nil
	}
	cmd, ok := commands[name]
	if !ok {
		return fmt.Errorf("unknown command %q\n\n%s", name, usageText)
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: truedns %s\n", cmd.usage)
	}
	if err := cmd.run(fs, args[1:]); err != nil {
		return err
	}
	return nil
}

// ---- shared helpers ----

// globalFlags are the flags shared by all commands.
type globalFlags struct {
	cfgPath   *string
	logLevel  *string
	logFile   *string
	noLogFile *bool
}

func addGlobalFlags(fs *flag.FlagSet) *globalFlags {
	return &globalFlags{
		cfgPath:   fs.String("config", "", "config file path (default: platform default)"),
		logLevel:  fs.String("log-level", "info", "log level: debug|info|warn|error"),
		logFile:   fs.String("log-file", "", "log file path (default: <config dir>/truedns.log)"),
		noLogFile: fs.Bool("no-log-file", false, "disable file logging"),
	}
}

// defaultLogPath is where serve/run append their logs. It lives next to the
// config/state directory so a crashed elevated window still leaves a trace.
func defaultLogPath() string {
	return filepath.Join(paths.StateDir(), "truedns.log")
}

// openLog opens the log file for append, rotating it once past 5 MiB.
func openLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// setupLogging configures slog for long-running commands: stderr always, plus
// the log file unless disabled (file failures only produce a warning).
func setupLogging(lvl, logFile string, noLogFile bool) error {
	var level slog.Level
	switch lvl {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("invalid log level %q (debug|info|warn|error)", lvl)
	}
	writers := []io.Writer{os.Stderr}
	if !noLogFile {
		path := logFile
		if path == "" {
			path = defaultLogPath()
		}
		if f, err := openLog(path); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot open log file %s: %v\n", path, err)
		} else {
			writers = append(writers, f)
			slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: level})))
			slog.Info("logging to file", "path", path)
			return nil
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: level})))
	return nil
}

func resolveConfigPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	return config.DefaultPath()
}

// maybeElevate relaunches elevated on Windows when needed. Returns true when
// the current process should exit (relaunch initiated).
func maybeElevate(noElevate bool) (bool, error) {
	if runtime.GOOS != "windows" || noElevate || platform.IsElevated() {
		return false, nil
	}
	relaunched, err := platform.Elevate()
	if err != nil {
		return false, err
	}
	return relaunched, nil
}

// takeoverExtra feeds the system-DNS takeover state into the control API.
func takeoverExtra() map[string]any {
	active, desc, err := platform.StateSummary()
	m := map[string]any{"active": active}
	if err != nil {
		m["error"] = err.Error()
	} else if active {
		m["description"] = desc
	}
	return m
}

// apiBaseCandidates returns candidate base URLs for the control API, in
// discovery order: the persisted api-port file first (the actual bound port
// after fallback), then the configured listen address.
func apiBaseCandidates(cfg *config.Config) []string {
	var out []string
	if p, err := api.ReadPortFile(); err == nil && p > 0 {
		out = append(out, fmt.Sprintf("http://127.0.0.1:%d", p))
	}
	if cfg.API.Enabled {
		if _, _, err := net.SplitHostPort(cfg.API.Listen); err == nil {
			out = append(out, "http://"+cfg.API.Listen)
		}
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(out))
	for _, u := range out {
		if !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	return uniq
}

// firstReachableAPI probes the candidate endpoints and returns the first base
// URL whose /healthz answers.
func firstReachableAPI(cfg *config.Config, client *http.Client) (string, error) {
	var lastErr error
	for _, base := range apiBaseCandidates(cfg) {
		req, _ := http.NewRequest(http.MethodGet, base+"/healthz", nil)
		if cfg.API.Token != "" {
			req.Header.Set("X-TrueDNS-Token", cfg.API.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return base, nil
		}
		lastErr = fmt.Errorf("%s returned %s", base, resp.Status)
	}
	return "", lastErr
}

// loadConfig loads the configuration for path. When path is the platform
// default location and the file is missing, it bootstraps the file with the
// built-in defaults (first-run experience); an explicitly given --config path
// that does not exist is still a hard error.
func loadConfig(path string, explicit bool) (*config.Config, error) {
	if _, err := os.Stat(path); err == nil {
		return config.Load(path)
	}
	if explicit {
		return nil, fmt.Errorf("config file not found: %s", path)
	}
	cfg := config.Default()
	if err := config.Write(path, cfg); err != nil {
		slog.Warn("could not write the default config, running with built-in defaults",
			"path", path, "err", err)
	} else {
		slog.Info("wrote default config (edit it to customize)", "path", path)
	}
	return cfg, nil
}

// startProxy builds the engine for an already-loaded configuration and
// (optionally) starts the control API. It returns the engine, the API server
// and a reload closure.
func startProxy(cfgPath string, cfg *config.Config) (*core.Engine, *api.Server, error) {
	eng, err := core.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	var apiSrv *api.Server
	if cfg.API.Enabled {
		apiSrv = api.New(cfg.API.Listen, cfg.API.Token, eng, func() error {
			nc, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			return eng.Reload(nc)
		})
		apiSrv.SetExtraStatus(takeoverExtra)
		if err := apiSrv.Start(); err != nil {
			// The API is auxiliary (tray GUI/status): a busy port must not
			// take the DNS proxy down.
			slog.Warn("control API unavailable for this session", "err", err)
			apiSrv = nil
		} else {
			slog.Info("control API listening", "addr", apiSrv.Addr())
		}
	}
	return eng, apiSrv, nil
}

func serveLoop(eng *core.Engine, apiSrv *api.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if apiSrv != nil {
		// Tray GUI / API can stop the proxy gracefully; the run process then
		// restores the system DNS via its deferred restore.
		apiSrv.SetShutdown(stop)
		defer func() {
			shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = apiSrv.Shutdown(shCtx)
		}()
	}
	st := eng.Status()
	slog.Info("true-dns proxy started",
		"version", version.Version,
		"mode", st.Mode,
		"listen", st.Listen,
		"doh_upstreams", st.DoHUpstreams,
		"system_upstreams", st.SystemUpstreams,
		"system_fallback_upstreams", st.SystemFallbackUpstreams,
		"polluted_domains", len(st.PollutedDomains),
	)
	// Periodic stats heartbeat so the window/log shows activity without
	// per-query verbosity (log.stats_interval, 0 disables).
	if iv := eng.StatsInterval(); iv > 0 {
		go func() {
			t := time.NewTicker(iv)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					s := eng.Status()
					slog.Info("stats",
						"uptime", time.Duration(s.UptimeSeconds)*time.Second,
						"queries", s.Queries,
						"doh", s.DoHQueries,
						"system", s.SystemQueries,
						"failures", s.Failures,
						"cache_entries", s.CacheSize,
						"cache_hits", s.CacheHits,
						"cache_misses", s.CacheMisses,
					)
				}
			}
		}()
	}
	return eng.Serve(ctx)
}

// ---- commands ----

func cmdServe(fs *flag.FlagSet, args []string) error {
	g := addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := setupLogging(*g.logLevel, *g.logFile, *g.noLogFile); err != nil {
		return err
	}
	path := resolveConfigPath(*g.cfgPath)
	cfg, err := loadConfig(path, *g.cfgPath != "")
	if err != nil {
		return err
	}
	eng, apiSrv, err := startProxy(path, cfg)
	if err != nil {
		return err
	}
	return serveLoop(eng, apiSrv)
}

func cmdRun(fs *flag.FlagSet, args []string) error {
	g := addGlobalFlags(fs)
	keep := fs.Bool("keep", false, "leave the system DNS takeover in place on exit")
	noElevate := fs.Bool("no-elevate", false, "do not attempt automatic UAC elevation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := setupLogging(*g.logLevel, *g.logFile, *g.noLogFile); err != nil {
		return err
	}
	relaunched, err := maybeElevate(*noElevate)
	if err != nil {
		return err
	}
	if relaunched {
		fmt.Println("UAC prompt shown — the elevated instance runs in a new console window; this one exits now.")
		fmt.Printf("If that window closes immediately, see the log: %s (also \"truedns logs\"), or run the same command from an elevated terminal.\n", defaultLogPath())
		return nil
	}
	slog.Info("privileges OK", "elevated", platform.IsElevated())

	// Load (and on first run, create) the config before touching the system
	// DNS, so config errors never leave the system pointing at a dead proxy.
	path := resolveConfigPath(*g.cfgPath)
	cfg, err := loadConfig(path, *g.cfgPath != "")
	if err != nil {
		return err
	}

	st, err := platform.Current().SetSystemDNS()
	if err != nil {
		return err
	}
	if err := platform.SaveState(st); err != nil {
		slog.Warn("could not persist takeover state (restore still works in-process)", "err", err)
	}
	slog.Info("system DNS taken over", "summary", platform.Current().DescribeState(st))
	if err := platform.Current().FlushDNSCache(); err != nil {
		slog.Warn("failed to flush system DNS cache", "err", err)
	}
	defer func() {
		if *keep {
			slog.Info("keeping the takeover in place (revert with \"truedns restore\")")
			return
		}
		if err := platform.Current().RestoreSystemDNS(st); err != nil {
			slog.Error("failed to restore system DNS", "err", err, "hint", "run \"truedns restore\"")
			return
		}
		_ = platform.ClearState()
		_ = platform.Current().FlushDNSCache()
		slog.Info("system DNS restored")
	}()

	eng, apiSrv, err := startProxy(path, cfg)
	if err != nil {
		return err
	}
	return serveLoop(eng, apiSrv)
}

func cmdSetup(fs *flag.FlagSet, args []string) error {
	g := addGlobalFlags(fs) // setup is config-independent
	force := fs.Bool("force", false, "re-apply the takeover even if one is already recorded")
	noElevate := fs.Bool("no-elevate", false, "do not attempt automatic UAC elevation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := setupLogging(*g.logLevel, *g.logFile, *g.noLogFile); err != nil {
		return err
	}
	relaunched, err := maybeElevate(*noElevate)
	if err != nil {
		return err
	}
	if relaunched {
		fmt.Println("UAC prompt shown — the elevated instance runs in a new console window; this one exits now.")
		return nil
	}
	slog.Info("privileges OK", "elevated", platform.IsElevated())
	if active, desc, err := platform.StateSummary(); err != nil {
		return err
	} else if active && !*force {
		return fmt.Errorf("system DNS already taken over (%s). Use --force to re-apply, or \"truedns restore\" to revert first", desc)
	}
	st, err := platform.Current().SetSystemDNS()
	if err != nil {
		return err
	}
	if err := platform.SaveState(st); err != nil {
		return fmt.Errorf("takeover applied but state could not be saved: %w", err)
	}
	if err := platform.Current().FlushDNSCache(); err != nil {
		slog.Warn("failed to flush system DNS cache", "err", err)
	}
	fmt.Printf("system DNS taken over: %s\n", platform.Current().DescribeState(st))
	fmt.Println("start the proxy with \"truedns run\" or \"truedns serve\" — until it runs, name resolution will fail.")
	fmt.Println("revert with \"truedns restore\".")
	return nil
}

func cmdRestore(fs *flag.FlagSet, args []string) error {
	noElevate := fs.Bool("no-elevate", false, "do not attempt automatic UAC elevation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	relaunched, err := maybeElevate(*noElevate)
	if err != nil {
		return err
	}
	if relaunched {
		fmt.Println("UAC prompt shown — the elevated instance runs in a new console window; this one exits now.")
		return nil
	}
	slog.Info("privileges OK", "elevated", platform.IsElevated())
	st, err := platform.LoadState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no takeover state found at %s — nothing to restore", platform.StateFilePath())
		}
		return err
	}
	if err := platform.Current().RestoreSystemDNS(st); err != nil {
		return err
	}
	if err := platform.ClearState(); err != nil {
		slog.Warn("state file could not be removed", "err", err)
	}
	_ = platform.Current().FlushDNSCache()
	fmt.Println("system DNS restored.")
	return nil
}

func cmdStatus(fs *flag.FlagSet, args []string) error {
	g := addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	active, desc, err := platform.StateSummary()
	if err != nil {
		return err
	}
	if active {
		fmt.Printf("接管状态: 已接管 — %s\n", desc)
	} else {
		fmt.Println("接管状态: 未接管 (系统 DNS 未被修改)")
	}

	path := resolveConfigPath(*g.cfgPath)
	fmt.Printf("配置文件: %s\n", path)
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("代理状态: 配置文件尚不存在 (运行 truedns run 将自动生成并启动)")
		} else {
			fmt.Printf("代理状态: 无法读取配置: %v\n", err)
		}
		return nil
	}
	if !cfg.API.Enabled {
		fmt.Println("代理状态: 控制 API 已禁用, 无法探测 (api.enabled = false)")
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	url, lastErr := firstReachableAPI(cfg, client)
	if url == "" {
		fmt.Printf("代理状态: 未运行或不可达 (%v)\n", lastErr)
		return nil
	}
	req, _ := http.NewRequest(http.MethodGet, url+"/api/v1/status", nil)
	if cfg.API.Token != "" {
		req.Header.Set("X-TrueDNS-Token", cfg.API.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("代理状态: 未运行或不可达 (%v)\n", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("代理状态: API 返回 %s\n", resp.Status)
		return nil
	}
	var body struct {
		Status   string         `json:"status"`
		Engine   core.Status    `json:"engine"`
		Takeover map[string]any `json:"takeover"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	st := body.Engine
	fmt.Printf(`代理状态: %s
  版本:        %s
  模式:        %s
  上游策略:    %s
  DoH 上游:    %s
  系统上游:    %s
  污染域名数:  %d
  运行时长:    %s
  查询:        %d (DoH %d / 系统 %d, 失败 %d)
  缓存:        %d 条 (命中 %d / 未命中 %d)
`, body.Status, st.Version, st.Mode, st.Strategy,
		strings.Join(st.DoHUpstreams, ", "),
		strings.Join(st.SystemUpstreams, ", "),
		len(st.PollutedDomains),
		time.Duration(st.UptimeSeconds)*time.Second,
		st.Queries, st.DoHQueries, st.SystemQueries, st.Failures,
		st.CacheSize, st.CacheHits, st.CacheMisses,
	)
	if st.LastFailure != nil {
		fmt.Printf("  最近失败:    %s (%s)\n", st.LastFailure.Error, st.LastFailure.At.Format(time.RFC3339))
	}
	return nil
}

func cmdFlush(fs *flag.FlagSet, args []string) error {
	g := addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := resolveConfigPath(*g.cfgPath)
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if !cfg.API.Enabled {
		return errors.New("control API is disabled (api.enabled = false)")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	url, lastErr := firstReachableAPI(cfg, client)
	if url == "" {
		return fmt.Errorf("flush failed (proxy not running?): %w", lastErr)
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/api/v1/flush", nil)
	if cfg.API.Token != "" {
		req.Header.Set("X-TrueDNS-Token", cfg.API.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("flush failed (proxy not running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("flush failed: API returned %s", resp.Status)
	}
	fmt.Println("cache flushed.")
	return nil
}

func cmdConfig(fs *flag.FlagSet, args []string) error {
	cfgPath := fs.String("config", "", "config file path (default: platform default)")
	force := fs.Bool("force", false, "overwrite an existing config file (init only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("missing config subcommand: init|show|path")
	}
	switch rest[0] {
	case "path":
		fmt.Println(resolveConfigPath(*cfgPath))
		return nil
	case "show":
		cfg := config.Default()
		if _, err := config.Load(resolveConfigPath(*cfgPath)); err == nil {
			cfg, _ = config.Load(resolveConfigPath(*cfgPath))
		}
		return toml.NewEncoder(os.Stdout).Encode(cfg)
	case "init":
		path := resolveConfigPath(*cfgPath)
		if _, err := os.Stat(path); err == nil && !*force {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintf(f, "# true-dns configuration. See config.example.toml in the source tree\n")
		fmt.Fprintf(f, "# for the fully commented reference.\n\n")
		if err := toml.NewEncoder(f).Encode(config.Default()); err != nil {
			return err
		}
		fmt.Printf("config written to %s\n", path)
		return nil
	default:
		return fmt.Errorf("unknown config subcommand %q: init|show|path", rest[0])
	}
}

func cmdVersion(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Printf("true-dns %s (%s/%s, go %s)\n", version.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}

// cmdLogs prints the tail of the log file (default: <config dir>/truedns.log).
func cmdLogs(fs *flag.FlagSet, args []string) error {
	n := fs.Int("n", 50, "number of lines to show")
	logFile := fs.String("log-file", "", "log file path (default: <config dir>/truedns.log)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *logFile
	if path == "" {
		path = defaultLogPath()
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open log %s: %w (run \"truedns run\" once to create it)", path, err)
	}
	defer f.Close()
	// Keep only the last n lines without loading unbounded amounts of data.
	lines := make([]string, 0, *n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > *n {
			lines = lines[len(lines)-*n:]
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Printf("%s is empty\n", path)
		return nil
	}
	fmt.Printf("=== %s (last %d lines) ===\n", path, len(lines))
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}
