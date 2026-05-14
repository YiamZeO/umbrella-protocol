package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"umbrella_client/internal/client/config"
	"umbrella_client/internal/client/decoy"
	"umbrella_client/internal/client/hysteria"
	"umbrella_client/internal/client/torrent"
	"umbrella_client/internal/client/xtls"
	"umbrella_client/internal/logging"
	"umbrella_client/internal/settings"
	"umbrella_client/internal/storage"
	"umbrella_client/internal/tunnel"
	"umbrella_client/internal/ui"
	"umbrella_client/internal/windows"
)

func initAndStart(appSettings *settings.AppSettings, l *logging.LogsContainer, appRef fyne.App, isRunning *bool, ctx context.Context, startEnabled, stopEnabled binding.Bool, onceLog *sync.Once, dnsCache *storage.DnsCache) {
	// Set up logging first to capture all output
	onceLog.Do(func() {
		log.SetOutput(&logging.LogWriter{LogsContainer: l})
		log.SetFlags(0)
	})
	err := os.MkdirAll(appSettings.AppFilesDir, 0o755)
	if err != nil {
		l.AppendLog("[ERR] Failed to create directory: " + err.Error())
		finish("Status: Directory Error", isRunning, l, startEnabled, stopEnabled)
		return
	}

	l.AppendLog("Loading config.yaml from storage/disk...")

	configData, err := storage.LoadConfig(appSettings.AppFilesDir, appRef)
	if err != nil {
		l.AppendLog("[ERR] Cannot read config.yaml: " + err.Error())
		finish("Status: Config Error", isRunning, l, startEnabled, stopEnabled)
		return
	}

	l.AppendLog("Parsing configuration...")
	cfg, err := config.ParseConfig(configData)
	if err != nil {
		l.AppendLog("[ERR] Config parse failed: " + err.Error())
		finish("Status: Config Error", isRunning, l, startEnabled, stopEnabled)
		return
	}

	// Load phases data from storage/disk (required)
	phasesData, err := storage.LoadPhases(appSettings.AppFilesDir, appRef)
	if err != nil {
		l.AppendLog("[ERR] Cannot read phases.yml: " + err.Error())
		finish("Status: Phases Error", isRunning, l, startEnabled, stopEnabled)
		return
	}
	cfg.PhasesData = phasesData

	// Log what will be started
	l.AppendLog(fmt.Sprintf("Starting client on %s", cfg.ListenAddr))
	if cfg.DNSListen != "" {
		l.AppendLog(fmt.Sprintf("DNS server on %s", cfg.DNSListen))
	}
	if cfg.Shaper {
		l.AppendLog("Traffic shaping: enabled")
	}
	if cfg.DecoyTraffic {
		l.AppendLog("Decoy traffic: enabled")
		go decoy.StartGlobalDecoy(ctx)
	}

	// Start tunnel core if configured
	var pr *os.Process
	if appSettings.TunnelCorePath != "" {
		if pr, err = tunnel.StartTunnelCore(appSettings.TunnelCorePath, appSettings.TunnelCoreArgs, l); err != nil {
			l.AppendLog("[ERR] Failed to start tunnel core: " + err.Error())
			finish("Status: Tunnel Core Error", isRunning, l, startEnabled, stopEnabled)
			return
		}
	}

	// Run client in a goroutine
	go func() {
		defer func() {
			// Ensure we always call finish and handle panics
			if r := recover(); r != nil {
				l.AppendLog(fmt.Sprintf("[Panic] Client crashed: %v", r))
				finish("Status: Crashed", isRunning, l, startEnabled, stopEnabled)
			} else if *isRunning {
				// Normal exit
				l.AppendLog("Client stopped, updating status")
				finish("Status: Stopped", isRunning, l, startEnabled, stopEnabled)
			}
			if appSettings.TunnelCorePath != "" {
				if err := tunnel.StopTunnelCore(pr); err == nil {
					l.AppendLog("Tunnel core stopped")
				} else {
					l.AppendLog("[ERR] Tunnel core not stopped. Need a manual stop process. " + err.Error())
				}
			}
			err := storage.SaveDnsCache(dnsCache, appSettings.AppFilesDir)
			if err != nil {
				l.AppendLog("[ERR] Failed save dns cache: " + err.Error())
			}
		}()

		l.AppendLog("Starting client...")
		var start func(cfg *config.Config, ctx context.Context, appFilesDir string, dnsCache *storage.DnsCache) error
		switch cfg.Protocol {
		case "xtls":
			start = xtls.Start
		case "hysteria":
			start = hysteria.Start
		case "torrent":
			start = torrent.Start
		default:
			l.AppendLog("[ERR] Client failed to start: not valid protocol")
			finish("Status: Failed", isRunning, l, startEnabled, stopEnabled)
		}
		err = start(cfg, ctx, appSettings.AppFilesDir, dnsCache)
		if err != nil {
			if strings.Contains(err.Error(), "failed to listen on "+cfg.ListenAddr) {
				if runtime.GOOS != "android" {
					switch runtime.GOOS {
					case "windows":
						errR := exec.Command("net", "stop", "winnat").Run()
						if errR == nil {
							errR = exec.Command("net", "start", "winnat").Run()
						}
						if errR != nil {
							l.AppendLog("[ERR] Failed net command: " + errR.Error())
							finish("Status: Failed net command", isRunning, l, startEnabled, stopEnabled)
							return
						}
					case "linux":
						if errR := exec.Command("sh", "-c", "fuser -k 1080/tcp").Run(); errR != nil {
							l.AppendLog("[ERR] Failed net sh: " + errR.Error())
							finish("Status: Failed net sh", isRunning, l, startEnabled, stopEnabled)
							return
						}
					}
					err = start(cfg, ctx, appSettings.AppFilesDir, dnsCache)
				}
			}

			l.AppendLog(fmt.Sprintf("[ERR] Client failed to start: %v", err))
			if runtime.GOOS != "android" {
				var comm string
				switch runtime.GOOS {
				case "windows":
					comm = "net stop winnat && net start winnat"
				case "linux":
					comm = "sudo sh -c fuser -k 1080/tcp"
				}
				l.AppendLog(fmt.Sprintf("[ERR] If port problem try this command in terminal: '%s'", comm))
			}
			finish("Status: Failed", isRunning, l, startEnabled, stopEnabled)
		} else {
			l.AppendLog("Client stopped gracefully")
			// Only call finish if we're still running (not already stopped)
			if *isRunning {
				finish("Status: Stopped", isRunning, l, startEnabled, stopEnabled)
			}
		}
	}()
}

func finish(status string, isRunning *bool, l *logging.LogsContainer, startEnabled, stopEnabled binding.Bool) {
	*isRunning = false
	l.StatusBind.Set(status)
	startEnabled.Set(true)
	stopEnabled.Set(false)
	l.AppendLog(fmt.Sprintf("Client status: %s", strings.TrimPrefix(status, "Status: ")))
}

func CreateAndRun() {
	myApp := app.NewWithID("com.umbrella.client")
	// Use the converted theme from internal/ui by default
	myApp.Settings().SetTheme(ui.NewFyneTheme())
	window := myApp.NewWindow("Umbrella Client")
	window.SetMaster()

	var appFilesDir string
	// Determine paths on Main thread
	if runtime.GOOS == "android" {
		appFilesDir = "/data/data/com.umbrella.client/files"
		if myApp.Storage() != nil && myApp.Storage().RootURI() != nil {
			appFilesDir = myApp.Storage().RootURI().Path()
		}
	} else {
		home, _ := os.UserHomeDir()
		appFilesDir = filepath.Join(home, ".umbrella-client")
	}

	// Try to load an application icon (icon.png or icon.ico).
	// Look in the executable directory, then in the app files dir, then the working dir.
	// Setting both app and window icon helps on desktop platforms.
	func() {
		candidates := []string{}
		if ex, err := os.Executable(); err == nil {
			ed := filepath.Dir(ex)
			candidates = append(candidates,
				filepath.Join(ed, "icon.png"),
				filepath.Join(ed, "icon.ico"),
				filepath.Join(ed, "Icon.png"),
				filepath.Join(ed, "Icon.ico"),
			)
		}
		candidates = append(candidates,
			filepath.Join(appFilesDir, "icon.png"),
			filepath.Join(appFilesDir, "Icon.png"),
			filepath.Join(appFilesDir, "icon.ico"),
			"icon.png",
			"Icon.png",
			"icon.ico",
		)

		for _, p := range candidates {
			if p == "" {
				continue
			}
			info, err := os.Stat(p)
			if err != nil || info.IsDir() {
				continue
			}
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			res := fyne.NewStaticResource(filepath.Base(p), data)
			myApp.SetIcon(res)
			// Also set the window icon explicitly — some platforms prefer this.
			if window != nil {
				window.SetIcon(res)
			}
			return
		}
	}()

	// Load persistent UI settings (font size, theme, window size)
	appSettings := settings.LoadAppSettings(appFilesDir, myApp)
	// apply font size and theme from settings
	logTextSize := appSettings.LogsFontSize
	if appSettings.Theme != "" {
		ui.SetThemeByName(appSettings.Theme)
		myApp.Settings().SetTheme(ui.NewFyneTheme())
	}
	// apply UI font family and size
	if appSettings.UiFontSize > 0 {
		ui.SetUIFontSize(appSettings.UiFontSize)
		myApp.Settings().SetTheme(ui.NewFyneTheme())
	}
	if appSettings.UiFontName != "" {
		if b, name, err := storage.LoadUIFontByName(appSettings.UiFontName, appFilesDir); err == nil {
			ui.SetUIFontFromBytes(name, b)
			ui.SetUIMonoFontFromBytes(name, b)
			myApp.Settings().SetTheme(ui.NewFyneTheme())
		}
	}

	lg := logging.NewLogsContainer()

	// UI Components
	statusLabel := widget.NewLabelWithData(lg.StatusBind)
	statusLabel.Alignment = fyne.TextAlignCenter
	statusLabel.TextStyle = fyne.TextStyle{Bold: true}

	// Создаем виртуализированный список
	logList := widget.NewList(
		func() int {
			lg.LogMu.RLock()
			defer lg.LogMu.RUnlock()
			return len(lg.Logs)
		},
		func() fyne.CanvasObject {
			t := canvas.NewText("", ui.ColorToNRGBA(ui.CurrentTheme.Text))
			t.TextSize = logTextSize
			t.TextStyle.Monospace = true
			return t
		},
		func(id widget.ListItemID, item fyne.CanvasObject) {
			lg.LogMu.RLock()
			defer lg.LogMu.RUnlock()
			if id >= len(lg.Logs) {
				return
			}
			msg := lg.Logs[id]
			t := item.(*canvas.Text)
			t.Text = msg
			if strings.Contains(msg, "[ERR]") || strings.Contains(msg, "[ERROR]") {
				t.Color = ui.ColorToNRGBA(ui.CurrentTheme.Love)
			} else {
				t.Color = ui.ColorToNRGBA(ui.CurrentTheme.Text)
			}
			t.TextSize = logTextSize
			t.Refresh()
		},
	)

	logList.OnSelected = func(id widget.ListItemID) {
		lg.LogMu.RLock()
		if id < len(lg.Logs) {
			msg := lg.Logs[id]
			window.Clipboard().SetContent(msg)
			myApp.SendNotification(fyne.NewNotification("Copied", "Log line copied to clipboard"))
		}
		lg.LogMu.RUnlock()
		logList.Unselect(id)
	}

	copyBtn := widget.NewButtonWithIcon("Copy All", theme.ContentCopyIcon(), func() {
		lg.LogMu.RLock()
		val := strings.Join(lg.Logs, "\n")
		lg.LogMu.RUnlock()
		window.Clipboard().SetContent(val)
		myApp.SendNotification(fyne.NewNotification("Copied", "All logs copied"))
	})

	clearBtn := widget.NewButtonWithIcon("Clear", theme.DeleteIcon(), func() {
		lg.LogMu.Lock()
		lg.Logs = []string{}
		lg.LogMu.Unlock()
		fyne.Do(func() { logList.Refresh() })
	})

	// Font size controls for logs
	decBtn := widget.NewButtonWithIcon("A-", theme.ContentRemoveIcon(), func() {
		if logTextSize > 6 {
			logTextSize -= 1
			fyne.Do(func() { logList.Refresh() })
			// persist change
			appSettings.LogsFontSize = logTextSize
			go settings.SaveAppSettings(appSettings, myApp)
		}
	})
	incBtn := widget.NewButtonWithIcon("A+", theme.ContentAddIcon(), func() {
		if logTextSize < 48 {
			logTextSize += 1
			fyne.Do(func() { logList.Refresh() })
			// persist change
			appSettings.LogsFontSize = logTextSize
			go settings.SaveAppSettings(appSettings, myApp)
		}
	})

	// Пакетное обновление UI раз в секунду
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var lastUpdateCount uint64
		for range ticker.C {
			lg.LogMu.RLock()
			currentUpdateCount := lg.UpdateCount
			lg.LogMu.RUnlock()

			if currentUpdateCount != lastUpdateCount {
				lastUpdateCount = currentUpdateCount

				fyne.Do(func() { logList.Refresh() })
				fyne.Do(func() { logList.ScrollToBottom() })

			}
		}
	}()

	startBtn := widget.NewButtonWithIcon("Start", theme.MediaPlayIcon(), nil)
	stopBtn := widget.NewButtonWithIcon("Stop", theme.MediaStopIcon(), nil)
	stopBtn.Disable()

	// Initialize button state bindings
	startEnabled := binding.NewBool()
	stopEnabled := binding.NewBool()
	startEnabled.Set(true)
	stopEnabled.Set(false)

	startEnabled.AddListener(binding.NewDataListener(func() {
		if v, _ := startEnabled.Get(); v {
			startBtn.Enable()
		} else {
			startBtn.Disable()
		}
	}))
	stopEnabled.AddListener(binding.NewDataListener(func() {
		if v, _ := stopEnabled.Get(); v {
			stopBtn.Enable()
		} else {
			stopBtn.Disable()
		}
	}))

	var cancelFunc context.CancelFunc

	stopFunc := func() {
		if cancelFunc != nil {
			lg.AppendLog("Stopping client...")
			lg.StatusBind.Set("Status: Stopping...")
			startEnabled.Set(false)
			stopEnabled.Set(false)
			cancelFunc()
		} else {
			lg.AppendLog("[Warning] Stop requested but no active client")
			lg.StatusBind.Set("Status: Ready")
			startEnabled.Set(true)
			stopEnabled.Set(false)
		}
	}

	isRunning := false

	dnsCache, err := storage.LoadDnsCache(appSettings.AppFilesDir)
	if err != nil {
		lg.AppendLog("[ERR] Failed load dns cache. Created new: " + err.Error())
	}

	startBtn.OnTapped = func() {
		if isRunning {
			lg.AppendLog("[Warning] Start requested but client is already running")
			return
		}
		isRunning = true
		startEnabled.Set(false)
		stopEnabled.Set(true)
		lg.StatusBind.Set("Status: Starting...")
		ctx, cancel := context.WithCancel(context.Background())
		cancelFunc = cancel
		var onceLog sync.Once
		go initAndStart(appSettings, lg, myApp, &isRunning, ctx, startEnabled, stopEnabled, &onceLog, dnsCache)
		if appSettings.Timer > 0 {
			go func() {
				var (
					current string
					err     error
				)
				for range 10 {
					current, err = lg.StatusBind.Get()
					if err != nil {
						lg.AppendLog("[ERR] " + err.Error())
					}
					if strings.Contains(current, "Started") {
						break
					}
					time.Sleep(1 * time.Second)
				}
				if !strings.Contains(current, "Started") {
					lg.AppendLog("[ERR] " + "Client not started for 10 seconds")
				} else {
					for timeToStopM := appSettings.Timer - 1; timeToStopM >= 0 && isRunning; timeToStopM -= 1 {
						for timeToStopS := 59; timeToStopS >= 0 && isRunning; timeToStopS -= 1 {
							err = lg.StatusBind.Set(current + " [" + strconv.Itoa(timeToStopM) + "m " + strconv.Itoa(timeToStopS) + "s]")
							if err != nil {
								lg.AppendLog("[ERR] " + err.Error())
							}
							time.Sleep(1 * time.Second)
						}
					}
					if isRunning {
						stopFunc()
					}
				}
			}()
		}
	}

	stopBtn.OnTapped = stopFunc

	var presetsWin fyne.Window
	presetsBtn := widget.NewButtonWithIcon("Presets", theme.FileIcon(), func() {
		if presetsWin != nil {
			presetsWin.Close()
		}
		presetsWin = windows.NewPresetsWindow(myApp, appSettings, lg)
		presetsWin.Show()
	})

	var settingsWin fyne.Window
	settingsBtn := widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), func() {
		if settingsWin != nil {
			settingsWin.Close()
		}
		settingsWin = windows.NewSettingsWindow(myApp, appSettings, lg, dnsCache)
		settingsWin.Show()
	})

	// Layout
	top := container.NewVBox(
		statusLabel,
		container.NewGridWithColumns(4, startBtn, stopBtn, presetsBtn, settingsBtn),
	)
	bottom := container.NewGridWithColumns(4, copyBtn, clearBtn, decBtn, incBtn)

	window.SetContent(container.NewBorder(top, bottom, nil, nil, logList))
	// Restore window size from settings if present
	if appSettings.WindowWidth > 0 && appSettings.WindowHeight > 0 {
		window.Resize(fyne.NewSize(appSettings.WindowWidth, appSettings.WindowHeight))
	}

	lg.StatusBind.Set("Status: Ready")
	startEnabled.Set(true)
	stopEnabled.Set(false)

	lg.AppendLog("--- Umbrella Client Ready ---")
	if runtime.GOOS == "android" {
		lg.AppendLog("Running on Android")
	} else {
		lg.AppendLog("Running on " + runtime.GOOS)
	}
	lg.AppendLog("Storage: " + appFilesDir)
	lg.AppendLog("Click 'Start' to begin")

	// Save settings when window is closed
	window.SetOnClosed(func() {
		go settings.SaveAppSettings(appSettings, myApp)
	})

	// Periodically persist window size if it changes (saved in UI thread)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			fyne.Do(func() {
				sz := window.Canvas().Size()
				w := float32(sz.Width)
				h := float32(sz.Height)
				if w > 0 && h > 0 && (w != appSettings.WindowWidth || h != appSettings.WindowHeight) {
					appSettings.WindowWidth = w
					appSettings.WindowHeight = h
					go settings.SaveAppSettings(appSettings, myApp)
				}
			})
		}
	}()

	window.ShowAndRun()
}
