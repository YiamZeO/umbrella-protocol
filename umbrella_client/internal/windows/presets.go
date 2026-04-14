package windows

import (
	"umbrella_client/internal/dialogs"
	"umbrella_client/internal/logging"
	"umbrella_client/internal/settings"
	"umbrella_client/internal/ui"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func NewPresetsWindow(appRef fyne.App, appSettings *settings.AppSettings, l *logging.LogsContainer) fyne.Window {
	presetsEditor := appRef.NewWindow("Presets")
	presetsEditor.SetIcon(theme.FileIcon())

	accent := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Gold))
	accent.SetMinSize(fyne.NewSize(10, 2))
	titleLbl := widget.NewLabelWithStyle("Presets", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	iconW := widget.NewIcon(theme.FileIcon())
	header := container.NewHBox(iconW, widget.NewLabel(" "), titleLbl)
	headerBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	_ = container.NewStack(headerBg, container.NewPadded(header))

	saveNewBtn := widget.NewButton("Save New Preset", func() {
		dialogs.ShowSavePresetDialog(appRef, l, appSettings)
	})

	closeBtn := widget.NewButton("Close", func() {
		presetsEditor.Close()
	})

	currentPresetLbl := widget.NewLabel("Current preset: " + appSettings.CurrentPreset)
	currentPresetLbl.TextStyle = fyne.TextStyle{Bold: true}

	presetListBox := container.NewVBox()
	for i := 0; i < len(appSettings.Presets); i++ {
		name := appSettings.Presets[i]
		itemRow := container.NewHBox(
			widget.NewLabel(name),
			widget.NewButton("Load", func() {
				if err := appSettings.LoadPreset(name, appRef); err != nil {
					l.AppendLog("[Error] Failed to load preset: " + err.Error())
					dialogs.ShowStyledError(presetsEditor, "Load Error", err.Error())
					return
				}
				l.AppendLog("[System] Loaded preset: " + name)
				appRef.SendNotification(fyne.NewNotification("Preset Loaded", name))
				presetListBox.Refresh()
				currentPresetLbl.SetText("Current preset: " + appSettings.CurrentPreset)
			}),
			widget.NewButton("Delete", func() {
				if err := appSettings.DeletePreset(name, appRef); err != nil {
					l.AppendLog("[Error] Failed to delete preset: " + err.Error())
					dialogs.ShowStyledError(presetsEditor, "Delete Error", err.Error())
					return
				}
				l.AppendLog("[System] Deleted preset: " + name)
				presetListBox.Objects = nil
				for _, n := range appSettings.Presets {
					itemRow := container.NewHBox(
						widget.NewLabel(n),
						widget.NewButton("Load", func() {}),
						widget.NewButton("Delete", func() {}),
					)
					presetListBox.Add(itemRow)
				}
				presetListBox.Refresh()
				currentPresetLbl.SetText("Current preset: " + appSettings.CurrentPreset)
			}),
		)
		if name == appSettings.CurrentPreset {
			itemRow.Objects[0].(*widget.Label).TextStyle = fyne.TextStyle{Bold: true}
		}
		presetListBox.Add(itemRow)
	}

	presetScroll := container.NewVScroll(presetListBox)
	presetScroll.SetMinSize(fyne.NewSize(450, 250))

	bottom := container.NewGridWithColumns(2, saveNewBtn, closeBtn)

	body := container.NewBorder(
		container.NewVBox(currentPresetLbl),
		bottom,
		nil,
		nil,
		presetScroll,
	)
	presetsEditor.SetContent(body)
	presetsEditor.Resize(fyne.NewSize(500, 400))
	presetsEditor.CenterOnScreen()

	return presetsEditor
}
