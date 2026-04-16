package app

import (
	"context"
	"fmt"
	"log"
	"math"
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
	"github.com/muesli/reflow/wordwrap"

	"umbrella_client"
	"umbrella_client/internal/client"
	"umbrella_client/internal/logging"
	"umbrella_client/internal/settings"
	"umbrella_client/internal/storage"
	"umbrella_client/internal/tunnel"
	"umbrella_client/internal/ui"
	"umbrella_client/internal/windows"
)

func initAndStart(appSettings *settings.AppSettings, l *logging.LogsContainer, appRef fyne.App, isRunning *bool, ctx context.Context, startEnabled, stopEnabled binding.Bool, onceLog *sync.Once) {
	// Set up logging first to capture all output
	onceLog.Do(func() {
		log.SetOutput(&logging.LogWriter{LogsContainer: l})
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	})
	err := os.MkdirAll(appSettings.AppFilesDir, 0o755)
	if err != nil {
		l.AppendLog("[Error] Failed to create directory: " + err.Error())
		finish("Status: Directory Error", isRunning, l, startEnabled, stopEnabled)
		return
	}

	l.AppendLog("[System] Loading config.yaml from storage/disk...")

	configData, err := storage.LoadConfig(appSettings.AppFilesDir, appRef)
	if err != nil {
		l.AppendLog("[Error] Cannot read config.yaml: " + err.Error())
		finish("Status: Config Error", isRunning, l, startEnabled, stopEnabled)
		return
	}

	l.AppendLog("[System] Parsing configuration...")
	cfg, err := client.ParseConfig(configData)
	if err != nil {
		l.AppendLog("[Error] Config parse failed: " + err.Error())
		finish("Status: Config Error", isRunning, l, startEnabled, stopEnabled)
		return
	}

	// Load phases data from storage/disk (required)
	phasesData, err := storage.LoadPhases(appSettings.AppFilesDir, appRef)
	if err != nil {
		l.AppendLog("[Error] Cannot read phases.yml: " + err.Error())
		finish("Status: Phases Error", isRunning, l, startEnabled, stopEnabled)
		return
	}
	cfg.PhasesData = phasesData

	// Keep decoy_reqs.json as optional embedded fallback
	if d, err := umbrella_client.FS.ReadFile("decoy_reqs.json"); err == nil {
		cfg.DecoyData = d
	}

	// Log what will be started
	l.AppendLog(fmt.Sprintf("[System] Starting client on %s", cfg.ListenAddr))
	if cfg.DNSListen != "" {
		l.AppendLog(fmt.Sprintf("[System] DNS server on %s", cfg.DNSListen))
	}
	if cfg.Shaper {
		l.AppendLog("[System] Traffic shaping: enabled")
	}
	if cfg.DecoyTraffic {
		l.AppendLog("[System] Decoy traffic: enabled")
	}

	// Start tunnel core if configured
	var pr *os.Process
	if appSettings.TunnelCorePath != "" {
		if pr, err = tunnel.StartTunnelCore(appSettings.TunnelCorePath, appSettings.TunnelCoreArgs, l); err != nil {
			l.AppendLog("[Error] Failed to start tunnel core: " + err.Error())
			finish("Status: Tunnel Core Error", isRunning, l, startEnabled, stopEnabled)
			return
		}
	}

	// Run client in a goroutine
	go func() {
		dnsCache, err := storage.LoadDnsCache(appSettings.AppFilesDir)
		if err != nil {
			l.AppendLog("[Error] Failed load dns cache: " + err.Error())
			finish("Status: Failed load dns cache", isRunning, l, startEnabled, stopEnabled)
			return
		}

		defer func() {
			// Ensure we always call finish and handle panics
			if r := recover(); r != nil {
				l.AppendLog(fmt.Sprintf("[Panic] Client crashed: %v", r))
				finish("Status: Crashed", isRunning, l, startEnabled, stopEnabled)
			} else if *isRunning {
				// Normal exit
				l.AppendLog("[System] Client stopped, updating status")
				finish("Status: Stopped", isRunning, l, startEnabled, stopEnabled)
			}
			if appSettings.TunnelCorePath != "" {
				if err := tunnel.StopTunnelCore(pr); err == nil {
					l.AppendLog("[System] Tunnel core stopped")
				} else {
					l.AppendLog("[ERR] Tunnel core not stopped. Need a manual stop process. " + err.Error())
				}
			}
			err := storage.SaveDnsCache(dnsCache, appSettings.AppFilesDir)
			if err != nil {
				l.AppendLog("[Error] Failed save dns cache: " + err.Error())
			}
		}()

		l.AppendLog("[System] Starting client...")
		err = client.Start(cfg, ctx, appSettings.AppFilesDir, dnsCache)
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
							l.AppendLog("[Error] Failed net command: " + errR.Error())
							finish("Status: Failed net command", isRunning, l, startEnabled, stopEnabled)
							return
						}
					case "linux":
						if errR := exec.Command("sh", "-c", "fuser -k 1080/tcp").Run(); errR != nil {
							l.AppendLog("[Error] Failed net sh: " + errR.Error())
							finish("Status: Failed net sh", isRunning, l, startEnabled, stopEnabled)
							return
						}
					}
					err = client.Start(cfg, ctx, appSettings.AppFilesDir, dnsCache)
				}
			}
			l.AppendLog(fmt.Sprintf("[Error] Client failed to start: %v", err))
			finish("Status: Failed", isRunning, l, startEnabled, stopEnabled)
		} else {
			l.AppendLog("[System] Client stopped gracefully")
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
	l.AppendLog(fmt.Sprintf("[System] Client status: %s", strings.TrimPrefix(status, "Status: ")))
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

	lg := logging.LogsContainer{
		LogMu:      sync.Mutex{},
		LogBind:    binding.NewString(),
		StatusBind: binding.NewString(),
	}

	// UI Components
	statusLabel := widget.NewLabelWithData(lg.StatusBind)
	statusLabel.Alignment = fyne.TextAlignCenter
	statusLabel.TextStyle = fyne.TextStyle{Bold: true}

	// Create a proper log display area as a scrollable VBox of colored lines
	logContent := container.NewVBox()
	logVBox := container.NewVScroll(logContent)

	rebuildMtx := sync.Mutex{}

	var lastColsWhenRebuild int

	// rebuildLogs renders `text` into the logContent using current wrapping and text size
	rebuildLogs := func(text string) {
		// Schedule the full rebuild on the Fyne main thread to avoid thread errors
		fyne.Do(func() {
			rebuildMtx.Lock()
			defer rebuildMtx.Unlock()
			if logVBox != nil {
				currentWidth := logVBox.Size().Width

				trimmed := strings.TrimRight(text, "\n")
				lines := []string{}
				if trimmed != "" {
					lines = strings.Split(trimmed, "\n")
				}
				if len(lines) > 100 {
					lines = lines[len(lines)-100:]
				}

				objs := []fyne.CanvasObject{}
				for _, line := range lines {
					col := ui.ColorToNRGBA(ui.CurrentTheme.Gold)
					if strings.Contains(line, "[ERR]") || strings.Contains(line, "[Error]") || strings.Contains(line, "[ERROR]") {
						col = ui.ColorToNRGBA(ui.CurrentTheme.Love)
					}

					// determine an approximate column width based on available pixels
					cols := 0
					if currentWidth > 0 {
						avgChar := float32(logTextSize) * 0.62
						if avgChar <= 0 {
							avgChar = 7.0
						}
						cols = int(math.Max(20, float64(currentWidth/avgChar)))
					}

					if cols <= 0 {
						if lastColsWhenRebuild > 0 {
							cols = lastColsWhenRebuild
						} else {
							cols = 120
						}
					} else {
						lastColsWhenRebuild = cols
					}

					wrapped := wordwrap.String(line, cols)
					sub := strings.Split(wrapped, "\n")
					for _, s := range sub {
						t := canvas.NewText(s, col)
						t.TextSize = logTextSize
						t.TextStyle.Monospace = true
						t.Alignment = fyne.TextAlignLeading
						objs = append(objs, t)
					}
				}

				logContent.Objects = objs
				logContent.Refresh()
				logVBox.ScrollToBottom()
			}
		})
	}

	// Connect the log binding to update the display (rebuilds line items)
	lg.LogBind.AddListener(binding.NewDataListener(func() {
		if text, err := lg.LogBind.Get(); err == nil {
			rebuildLogs(text)
		}
	}))

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
			lg.AppendLog("[System] Stopping client...")
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
		go initAndStart(appSettings, &lg, myApp, &isRunning, ctx, startEnabled, stopEnabled, &onceLog)
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

	copyBtn := widget.NewButtonWithIcon("Copy All", theme.ContentCopyIcon(), func() {
		val, _ := lg.LogBind.Get()
		window.Clipboard().SetContent(val)
		myApp.SendNotification(fyne.NewNotification("Copied", "All logs copied"))
	})

	clearBtn := widget.NewButtonWithIcon("Clear", theme.DeleteIcon(), func() {
		lg.LogMu.Lock()
		lg.AllLogs = ""
		lg.LogMu.Unlock()
		rebuildLogs("")
	})

	// Font size controls for logs
	decBtn := widget.NewButtonWithIcon("A-", theme.ContentRemoveIcon(), func() {
		if logTextSize > 6 {
			logTextSize -= 1
			rebuildLogs(lg.AllLogs)
			// persist change
			appSettings.LogsFontSize = logTextSize
			go settings.SaveAppSettings(appSettings, myApp)
		}
	})
	incBtn := widget.NewButtonWithIcon("A+", theme.ContentAddIcon(), func() {
		if logTextSize < 48 {
			logTextSize += 1
			rebuildLogs(lg.AllLogs)
			// persist change
			appSettings.LogsFontSize = logTextSize
			go settings.SaveAppSettings(appSettings, myApp)
		}
	})

	var presetsWin fyne.Window
	presetsBtn := widget.NewButtonWithIcon("Presets", theme.FileIcon(), func() {
		if presetsWin != nil {
			presetsWin.Close()
		}
		presetsWin = windows.NewPresetsWindow(myApp, appSettings, &lg)
		presetsWin.Show()
	})

	var settingsWin fyne.Window
	settingsBtn := widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), func() {
		if settingsWin != nil {
			settingsWin.Close()
		}
		settingsWin = windows.NewSettingsWindow(myApp, appSettings, &lg)
		settingsWin.Show()
	})

	// Layout
	top := container.NewVBox(
		statusLabel,
		container.NewGridWithColumns(4, startBtn, stopBtn, presetsBtn, settingsBtn),
	)
	bottom := container.NewGridWithColumns(4, copyBtn, clearBtn, decBtn, incBtn)

	window.SetContent(container.NewBorder(top, bottom, nil, nil, logVBox))
	// Restore window size from settings if present
	if appSettings.WindowWidth > 0 && appSettings.WindowHeight > 0 {
		window.Resize(fyne.NewSize(appSettings.WindowWidth, appSettings.WindowHeight))
	}

	// Watch for width changes and rewrap logs when needed
	var lastLogWidth float32
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if logVBox == nil {
				continue
			}
			w := logVBox.Size().Width
			if w <= 0 {
				continue
			}
			if math.Abs(float64(w-lastLogWidth)) > 1.0 {
				lastLogWidth = w
				if rebuildMtx.TryLock() {
					// trigger rebuild with new wrapping
					rebuildLogs(lg.AllLogs)
					rebuildMtx.Unlock()
				}

			}
		}
	}()

	lg.StatusBind.Set("Status: Ready")
	startEnabled.Set(true)
	stopEnabled.Set(false)

	lg.AppendLog("--- Umbrella Client Ready ---")
	if runtime.GOOS == "android" {
		lg.AppendLog("[System] Running on Android")
	} else {
		lg.AppendLog("[System] Running on " + runtime.GOOS)
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
