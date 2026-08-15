//go:build windows

package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/getlantern/systray"

	"truedns/internal/config"
	"truedns/internal/core"
	"truedns/internal/platform"
	"truedns/internal/version"
)

//go:embed assets/tray.ico
var trayIcon []byte

const trayPollInterval = 3 * time.Second

// cmdTray runs the Windows system-tray GUI. The proxy itself runs as a
// separate (elevated) process; the tray only controls it via the loopback
// control API, so quitting the tray never affects resolution.
func cmdTray(fs *flag.FlagSet, args []string) error {
	g := addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := setupLogging(*g.logLevel, *g.logFile, *g.noLogFile); err != nil {
		return err
	}
	cfgPath := resolveConfigPath(*g.cfgPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		cfg = config.Default() // tray works even before first run
	}
	st := &trayState{
		cfgPath: cfgPath,
		cfg:     cfg,
		client:  &http.Client{Timeout: 2 * time.Second},
	}
	systray.Run(st.onReady, st.onExit)
	return nil
}

type trayState struct {
	cfgPath string
	cfg     *config.Config
	client  *http.Client
	mStatus *systray.MenuItem
}

func (t *trayState) onReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("true-dns")
	systray.SetTooltip("true-dns")

	t.mStatus = systray.AddMenuItem("代理未运行", "当前状态")
	t.mStatus.Disable()
	systray.AddSeparator()
	mStart := systray.AddMenuItem("启动代理 (接管系统 DNS)", "以管理员权限运行 truedns run")
	mStop := systray.AddMenuItem("停止代理", "优雅停止并恢复系统 DNS")
	mFlush := systray.AddMenuItem("清空缓存", "经控制 API 清空解析缓存")
	mRestore := systray.AddMenuItem("恢复系统 DNS", "紧急恢复 (需管理员权限)")
	systray.AddSeparator()
	mOpenLog := systray.AddMenuItem("打开日志文件", "truedns.log")
	mOpenCfg := systray.AddMenuItem("打开配置文件", "config.toml")
	systray.AddSeparator()
	mAbout := systray.AddMenuItem(fmt.Sprintf("true-dns %s", version.Version), "关于")
	mAbout.Disable()
	mQuit := systray.AddMenuItem("退出托盘", "退出托盘 (代理不受影响)")

	go func() {
		for {
			select {
			case <-mStart.ClickedCh:
				t.startProxy()
			case <-mStop.ClickedCh:
				t.stopProxy()
			case <-mFlush.ClickedCh:
				t.flushCache()
			case <-mRestore.ClickedCh:
				t.restoreDNS()
			case <-mOpenLog.ClickedCh:
				t.openLog()
			case <-mOpenCfg.ClickedCh:
				t.openConfig()
			case <-mQuit.ClickedCh:
				systray.Quit()
			}
		}
	}()

	go t.pollLoop()
}

func (t *trayState) onExit() {}

func (t *trayState) pollLoop() {
	for range time.Tick(trayPollInterval) {
		t.refresh()
	}
}

// refresh queries the control API (with api-port discovery) and the takeover
// state file, updating the status menu item and tooltip.
func (t *trayState) refresh() {
	line := "代理未运行"
	tooltip := "true-dns: 代理未运行"
	base, err := firstReachableAPI(t.cfg, t.client)
	if err == nil {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/status", nil)
		if t.cfg.API.Token != "" {
			req.Header.Set("X-TrueDNS-Token", t.cfg.API.Token)
		}
		resp, err := t.client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				var body struct {
					Engine core.Status `json:"engine"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
					st := body.Engine
					line = fmt.Sprintf("运行中 | %s | 查询 %d | 失败 %d", st.Mode, st.Queries, st.Failures)
					tooltip = fmt.Sprintf(
						"true-dns %s\n模式 %s | 运行 %s\nDoH %d / 系统 %d / 回退 %d | 失败 %d\n缓存 %d 条 (命中 %d)",
						version.Version, st.Mode, time.Duration(st.UptimeSeconds)*time.Second,
						st.DoHQueries, st.SystemQueries, st.FallbackQueries, st.Failures,
						st.CacheSize, st.CacheHits)
				}
			}
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	if active, desc, err := platform.StateSummary(); err == nil && active {
		line += " | DNS 已接管"
		tooltip += "\n" + desc
	} else {
		line += " | DNS 未接管"
	}
	t.mStatus.SetTitle(line)
	systray.SetTooltip(tooltip)
}

// startProxy launches "truedns run" elevated (the run command performs the
// DNS takeover itself).
func (t *trayState) startProxy() {
	args := []string{"run", "--config", t.cfgPath}
	if !platform.IsElevated() {
		if _, err := platform.ElevateArgs(args); err != nil {
			t.mStatus.SetTitle("启动失败: " + err.Error())
		}
		return
	}
	t.spawnHidden(args...)
}

// restoreDNS launches "truedns restore" (self-elevating).
func (t *trayState) restoreDNS() {
	if !platform.IsElevated() {
		if _, err := platform.ElevateArgs([]string{"restore"}); err != nil {
			t.mStatus.SetTitle("恢复失败: " + err.Error())
		}
		return
	}
	t.spawnHidden("restore")
}

// stopProxy asks the control API to shut the proxy down gracefully; the run
// process restores the system DNS on exit.
func (t *trayState) stopProxy() {
	if err := t.post("/api/v1/shutdown"); err != nil {
		t.mStatus.SetTitle("停止失败: " + err.Error())
	}
}

// flushCache clears the proxy cache via the control API.
func (t *trayState) flushCache() {
	if err := t.post("/api/v1/flush"); err != nil {
		t.mStatus.SetTitle("清空缓存失败: " + err.Error())
	}
}

func (t *trayState) post(path string) error {
	base, err := firstReachableAPI(t.cfg, t.client)
	if err != nil {
		return fmt.Errorf("控制 API 不可达 (%w)", err)
	}
	req, _ := http.NewRequest(http.MethodPost, base+path, nil)
	if t.cfg.API.Token != "" {
		req.Header.Set("X-TrueDNS-Token", t.cfg.API.Token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API 返回 %s", resp.Status)
	}
	return nil
}

// spawnHidden runs the truedns executable detached with a hidden console;
// output still lands in the log file.
func (t *trayState) spawnHidden(args ...string) {
	exe, err := os.Executable()
	if err != nil {
		t.mStatus.SetTitle("启动失败: " + err.Error())
		return
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		t.mStatus.SetTitle("启动失败: " + err.Error())
	}
}

// openLog opens the log file in the default viewer (or its directory when
// the file does not exist yet).
func (t *trayState) openLog() {
	path := defaultLogPath()
	if _, err := os.Stat(path); err != nil {
		path = filepath.Dir(path)
	}
	t.shellOpen(path)
}

// openConfig opens the config file (or its directory when not created yet).
func (t *trayState) openConfig() {
	path := t.cfgPath
	if _, err := os.Stat(path); err != nil {
		path = filepath.Dir(path)
	}
	t.shellOpen(path)
}

func (t *trayState) shellOpen(path string) {
	_ = exec.Command("cmd", "/c", "start", "", path).Start()
}
