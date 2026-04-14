package windows

import (
	"fmt"
	"io"
	"runtime"
	"strconv"

	"umbrella_client/internal/dialogs"
	"umbrella_client/internal/logging"
	"umbrella_client/internal/settings"
	"umbrella_client/internal/storage"
	"umbrella_client/internal/ui"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	fstorage "fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func NewSettingsWindow(appRef fyne.App, appSettings *settings.AppSettings, l *logging.LogsContainer) fyne.Window {
	settingsWin := appRef.NewWindow("Settings")
	settingsWin.SetIcon(theme.SettingsIcon())

	accent := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Gold))
	accent.SetMinSize(fyne.NewSize(10, 2))
	titleLbl := widget.NewLabelWithStyle("Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	iconW := widget.NewIcon(theme.SettingsIcon())
	header := container.NewHBox(iconW, widget.NewLabel(" "), titleLbl)
	headerBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	headerStack := container.NewStack(headerBg, container.NewPadded(header))

	var configWin fyne.Window
	configBtn := widget.NewButtonWithIcon("Config", theme.DocumentCreateIcon(), func() {
		if configWin != nil {
			configWin.Close()
		}
		var data []byte
		if b, err := storage.LoadConfig(appSettings.AppFilesDir, appRef); err == nil {
			data = b
		} else {
			data = []byte("# Enter config.yaml here\n")
		}
		entry := widget.NewMultiLineEntry()
		entry.Wrapping = fyne.TextWrapWord
		entry.SetText(string(data))

		saveBtn := widget.NewButton("Save", func() {
			if err := storage.SaveConfig([]byte(entry.Text), appSettings.AppFilesDir, appRef); err != nil {
				l.AppendLog("[Error] Failed to save config.yaml: " + err.Error())
				dialogs.ShowStyledError(configWin, "Save Error", err.Error())
				return
			}
			l.AppendLog("[System] Saved config.yaml")
			appRef.SendNotification(fyne.NewNotification("Saved", "config.yaml saved"))
			if configWin != nil {
				configWin.Close()
			}
		})
		cancelBtn := widget.NewButton("Cancel", func() {
			if configWin != nil {
				configWin.Close()
			}
		})

		configWin = dialogs.NewEditorWindow("Edit config.yaml", theme.DocumentCreateIcon(), entry, appRef, saveBtn, cancelBtn)
		configWin.Show()
	})

	var phasesWin fyne.Window
	phasesBtn := widget.NewButtonWithIcon("Phases", theme.ListIcon(), func() {
		if phasesWin != nil {
			phasesWin.Close()
		}
		var data []byte
		if b, err := storage.LoadPhases(appSettings.AppFilesDir, appRef); err == nil {
			data = b
		} else {
			data = []byte("# Enter phases.yml here\n")
		}
		entry := widget.NewMultiLineEntry()
		entry.Wrapping = fyne.TextWrapWord
		entry.SetText(string(data))

		saveBtn := widget.NewButton("Save", func() {
			if err := storage.SavePhases([]byte(entry.Text), appSettings.AppFilesDir, appRef); err != nil {
				l.AppendLog("[Error] Failed to save phases.yml: " + err.Error())
				dialogs.ShowStyledError(phasesWin, "Save Error", err.Error())
				return
			}
			l.AppendLog("[System] Saved phases.yml")
			appRef.SendNotification(fyne.NewNotification("Saved", "phases.yml saved"))
			if phasesWin != nil {
				phasesWin.Close()
			}
		})
		cancelBtn := widget.NewButton("Cancel", func() {
			if phasesWin != nil {
				phasesWin.Close()
			}
		})

		phasesWin = dialogs.NewEditorWindow("Edit phases.yml", theme.ListIcon(), entry, appRef, saveBtn, cancelBtn)
		phasesWin.Show()
	})

	var themeWin fyne.Window
	themeBtn := widget.NewButtonWithIcon("Theme", theme.ColorPaletteIcon(), func() {
		if themeWin != nil {
			themeWin.Close()
		}
		opts := []string{}
		for _, t := range ui.AvailableThemes {
			opts = append(opts, t.Name)
		}
		sel := widget.NewSelect(opts, func(name string) {
			ui.SetThemeByName(name)
			fyne.Do(func() {
				appRef.Settings().SetTheme(ui.NewFyneTheme())
			})
			appSettings.Theme = name
			go settings.SaveAppSettings(appSettings, appRef)
		})
		sel.SetSelected(ui.CurrentTheme.Name)
		closeBtn := widget.NewButton("Close", func() {
			if themeWin != nil {
				themeWin.Close()
			}
		})
		themeWin = dialogs.NewEditorWindow("Select Theme", theme.ColorPaletteIcon(), container.NewVBox(sel), appRef, closeBtn)
		themeWin.Show()
	})

	var fontsWin fyne.Window
	fontsBtn := widget.NewButtonWithIcon("Fonts", theme.SettingsIcon(), func() {
		if fontsWin != nil {
			fontsWin.Close()
		}
		fonts := storage.DiscoverFonts(appSettings.AppFilesDir)
		opts := []string{"System Default"}
		for _, f := range fonts {
			opts = append(opts, f.Name)
		}
		selected := "System Default"
		if appSettings.UiFontName != "" {
			selected = appSettings.UiFontName
		}
		sel := widget.NewSelect(opts, func(_ string) {})
		sel.SetSelected(selected)
		sz := appSettings.UiFontSize
		if sz <= 0 {
			sz = 0
		}
		valLbl := widget.NewLabel("")
		sizeEntry := widget.NewEntry()
		sizeEntry.SetText(fmt.Sprintf("%.0f", sz))
		updateVal := func() {
			v := sizeEntry.Text
			if v == "" || v == "0" {
				valLbl.SetText("Default")
			} else {
				valLbl.SetText(v)
			}
		}
		updateVal()
		installBtn := widget.NewButton("Install...", func() {
			fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
				if err != nil || rc == nil {
					return
				}
				defer rc.Close()
				data, rerr := io.ReadAll(rc)
				if rerr != nil {
					return
				}
				name := rc.URI().Name()
				storage.SaveUIFontToFontsDir(name, data, appSettings.AppFilesDir)
				fonts = storage.DiscoverFonts(appSettings.AppFilesDir)
				fyne.Do(func() {
					opts = []string{"System Default"}
					for _, f := range fonts {
						opts = append(opts, f.Name)
					}
					sel.Options = opts
					sel.Refresh()
				})
			}, fontsWin)
			fd.SetFilter(fstorage.NewExtensionFileFilter([]string{".ttf", ".otf"}))
			fd.Show()
		})
		saveBtn := widget.NewButton("Apply", func() {
			chosen := sel.Selected
			newSize := sizeEntry.Text
			parsedSize := float32(0)
			if newSize != "" && newSize != "0" {
				if v, err := strconv.ParseFloat(newSize, 32); err == nil {
					parsedSize = float32(v)
				}
			}
			if chosen == "System Default" || chosen == "" {
				ui.ClearUIFont()
				appSettings.UiFontName = ""
			} else {
				if b, name, err := storage.LoadUIFontByName(chosen, appSettings.AppFilesDir); err == nil {
					ui.SetUIFontFromBytes(name, b)
					ui.SetUIMonoFontFromBytes(name, b)
					appSettings.UiFontName = name
				}
			}
			if parsedSize > 0 {
				ui.SetUIFontSize(parsedSize)
				appSettings.UiFontSize = parsedSize
			} else {
				ui.SetUIFontSize(0)
				appSettings.UiFontSize = 0
			}
			fyne.Do(func() {
				appRef.Settings().SetTheme(ui.NewFyneTheme())
			})
			go settings.SaveAppSettings(appSettings, appRef)
		})
		closeBtn := widget.NewButton("Close", func() {
			if fontsWin != nil {
				fontsWin.Close()
			}
		})
		body := container.NewVBox(
			widget.NewLabel("Font family"),
			sel,
			widget.NewLabel("UI text size"),
			container.NewGridWithColumns(2, sizeEntry, valLbl),
			installBtn,
		)
		fontsWin = dialogs.NewEditorWindow("Fonts", theme.SettingsIcon(), body, appRef, saveBtn, closeBtn)
		fontsWin.Show()
	})

	var presetsWin fyne.Window
	presetsBtn := widget.NewButtonWithIcon("Presets", theme.FileIcon(), func() {
		if presetsWin != nil {
			presetsWin.Close()
		}
		presetsWin = NewPresetsWindow(appRef, appSettings, l)
		presetsWin.Show()
	})

	var tunnelCoreWin fyne.Window
	tunnelCoreBtn := widget.NewButtonWithIcon("Tunnel core", theme.ConfirmIcon(), func() {
		if tunnelCoreWin != nil {
			tunnelCoreWin.Close()
		}
		tunnelCoreWin = NewTunnelCoreWindow(appRef, appSettings, l)
		tunnelCoreWin.Show()
	})

	closeBtn := widget.NewButton("Close", func() {
		settingsWin.Close()
	})

	body := container.NewVBox(
		configBtn,
		phasesBtn,
		themeBtn,
		fontsBtn,
		presetsBtn,
	)

	if runtime.GOOS != "android" {
		body.Add(tunnelCoreBtn)
	}

	var bottom fyne.CanvasObject
	bottomBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	bottom = container.NewStack(bottomBg, container.NewPadded(container.NewGridWithColumns(1, closeBtn)))

	content := container.NewBorder(container.NewVBox(accent, headerStack), bottom, nil, nil, body)
	settingsWin.SetContent(content)
	settingsWin.Resize(fyne.NewSize(400, 350))
	settingsWin.CenterOnScreen()
	return settingsWin
}

func NewTimerWindow(appRef fyne.App, appSettings *settings.AppSettings) fyne.Window {
	timerEditor := appRef.NewWindow("Timer Settings")
	timerEditor.SetIcon(theme.CalendarIcon())

	accent := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Gold))
	accent.SetMinSize(fyne.NewSize(10, 2))
	titleLbl := widget.NewLabelWithStyle("Timer", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	iconW := widget.NewIcon(theme.CalendarIcon())
	header := container.NewHBox(iconW, widget.NewLabel(" "), titleLbl)
	headerBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	headerStack := container.NewStack(headerBg, container.NewPadded(header))

	valLbl := widget.NewLabel("")
	timerEntry := widget.NewEntry()
	timerEntry.SetText(fmt.Sprintf("%d", appSettings.Timer))
	updateVal := func() {
		v := timerEntry.Text
		if v == "" || v == "0" {
			valLbl.SetText("Off (0 min)")
		} else {
			valLbl.SetText(v + " minutes")
		}
	}
	updateVal()

	body := container.NewVBox(
		widget.NewLabel("Auto-stop after N minutes (0 is off)"),
		container.NewGridWithColumns(2, timerEntry, valLbl),
	)

	closeBtn := widget.NewButton("Close", func() {
		timerEditor.Close()
	})

	var bottom fyne.CanvasObject
	bottomBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	bottom = container.NewStack(bottomBg, container.NewPadded(container.NewGridWithColumns(1, closeBtn)))

	content := container.NewBorder(container.NewVBox(accent, headerStack), bottom, nil, nil, body)
	timerEditor.SetContent(content)
	timerEditor.Resize(fyne.NewSize(400, 180))
	timerEditor.CenterOnScreen()

	closeBtn.OnTapped = func() {
		v := timerEntry.Text
		if parsed, err := strconv.Atoi(v); err == nil {
			appSettings.Timer = parsed
		} else {
			appSettings.Timer = 0
		}
		go settings.SaveAppSettings(appSettings, appRef)
		timerEditor.Close()
	}

	return timerEditor
}
