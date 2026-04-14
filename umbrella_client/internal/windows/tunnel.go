package windows

import (
	"strings"

	"umbrella_client/internal/dialogs"
	"umbrella_client/internal/logging"
	"umbrella_client/internal/settings"
	"umbrella_client/internal/ui"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func NewTunnelCoreWindow(appRef fyne.App, appSettings *settings.AppSettings, l *logging.LogsContainer) fyne.Window {
	tunnelCoreEditor := appRef.NewWindow("Tunnel Core")
	tunnelCoreEditor.SetIcon(theme.ConfirmIcon())

	accent := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Gold))
	accent.SetMinSize(fyne.NewSize(10, 2))
	titleLbl := widget.NewLabelWithStyle("Tunnel Core", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	iconW := widget.NewIcon(theme.ConfirmIcon())
	header := container.NewHBox(iconW, widget.NewLabel(" "), titleLbl)
	headerBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	headerStack := container.NewStack(headerBg, container.NewPadded(header))

	pathEntry := widget.NewEntry()
	pathEntry.SetText(appSettings.TunnelCorePath)
	pathEntry.SetPlaceHolder("Path to executable")

	argsEntry := widget.NewEntry()
	argsEntry.SetText(appSettings.TunnelCoreArgs)
	argsEntry.SetPlaceHolder("Arguments (e.g., -c config.yaml)")

	browseBtn := widget.NewButton("Browse...", func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
			if err != nil || rc == nil {
				return
			}
			defer rc.Close()
			pathEntry.SetText(rc.URI().Path())
		}, tunnelCoreEditor)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".exe", ".bin", ""}))
		fd.Show()
	})

	saveBtn := widget.NewButton("Save", func() {
		appSettings.TunnelCorePath = strings.TrimSpace(pathEntry.Text)
		appSettings.TunnelCoreArgs = strings.TrimSpace(argsEntry.Text)
		if err := settings.SaveAppSettings(appSettings, appRef); err != nil {
			l.AppendLog("[Error] Failed to save tunnel core settings: " + err.Error())
			dialogs.ShowStyledError(tunnelCoreEditor, "Save Error", err.Error())
			return
		}
		l.AppendLog("[System] Tunnel core settings saved")
		appRef.SendNotification(fyne.NewNotification("Saved", "Tunnel core settings saved"))
		tunnelCoreEditor.Close()
	})

	cancelBtn := widget.NewButton("Cancel", func() {
		tunnelCoreEditor.Close()
	})

	body := container.NewVBox(
		widget.NewLabel("Executable path:"),
		container.NewGridWithColumns(2, pathEntry, browseBtn),
		widget.NewLabel("Arguments:"),
		argsEntry,
	)

	bottom := container.NewGridWithColumns(2, saveBtn, cancelBtn)

	content := container.NewBorder(container.NewVBox(accent, headerStack), bottom, nil, nil, body)
	tunnelCoreEditor.SetContent(content)
	tunnelCoreEditor.Resize(fyne.NewSize(500, 220))
	tunnelCoreEditor.CenterOnScreen()

	return tunnelCoreEditor
}
