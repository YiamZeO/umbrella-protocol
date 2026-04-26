package dialogs

import (
	"strings"

	"umbrella_client/internal/logging"
	"umbrella_client/internal/settings"
	"umbrella_client/internal/ui"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func newStyledDialog(title string, icon fyne.Resource, body fyne.CanvasObject, win fyne.Window, actions ...fyne.CanvasObject) dialog.Dialog {
	accent := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Gold))
	accent.SetMinSize(fyne.NewSize(10, 2))
	titleLbl := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	iconW := widget.NewIcon(icon)
	header := container.NewHBox(iconW, widget.NewLabel(" "), titleLbl)
	headerBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	headerStack := container.NewStack(headerBg, container.NewPadded(header))
	bodyScroll := container.NewVScroll(body)
	bodyPad := container.NewPadded(bodyScroll)
	bodyBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Surface))
	bodyStack := container.NewStack(bodyBg, bodyPad)
	var bottom fyne.CanvasObject
	if len(actions) > 0 {
		actionsBar := container.NewGridWithColumns(len(actions), actions...)
		bottomBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
		bottom = container.NewStack(bottomBg, container.NewPadded(actionsBar))
	}
	content := container.NewBorder(container.NewVBox(accent, headerStack), bottom, nil, nil, bodyStack)
	d := dialog.NewCustomWithoutButtons("", content, win)
	return d
}

func NewEditorWindow(title string, icon fyne.Resource, body fyne.CanvasObject, appRef fyne.App, actions ...fyne.CanvasObject) fyne.Window {
	editorWin := appRef.NewWindow(title)
	editorWin.SetIcon(icon)

	accent := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Gold))
	accent.SetMinSize(fyne.NewSize(10, 2))
	titleLbl := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	iconW := widget.NewIcon(icon)
	header := container.NewHBox(iconW, widget.NewLabel(" "), titleLbl)
	headerBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	headerStack := container.NewStack(headerBg, container.NewPadded(header))
	bodyScroll := container.NewVScroll(body)
	bodyPad := container.NewPadded(bodyScroll)
	bodyBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Surface))
	bodyStack := container.NewStack(bodyBg, bodyPad)
	var bottom fyne.CanvasObject
	if len(actions) > 0 {
		actionsBar := container.NewGridWithColumns(len(actions), actions...)
		bottomBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
		bottom = container.NewStack(bottomBg, container.NewPadded(actionsBar))
	}
	content := container.NewBorder(container.NewVBox(accent, headerStack), bottom, nil, nil, bodyStack)
	editorWin.SetContent(content)
	editorWin.Resize(fyne.NewSize(700, 500))
	editorWin.CenterOnScreen()
	return editorWin
}

func ShowStyledError(win fyne.Window, title, message string) {
	iconRes := theme.DeleteIcon()
	lbl := widget.NewLabel(message)
	lbl.Wrapping = fyne.TextWrapWord
	okBtn := widget.NewButton("OK", func() {})
	d := newStyledDialog(title, iconRes, container.NewVBox(lbl), win, okBtn)
	okBtn.OnTapped = func() { d.Hide() }
	d.Resize(fyne.NewSize(520, 220))
	d.Show()
}

func ShowSavePresetDialog(appRef fyne.App, logsContainer *logging.LogsContainer, appSettings *settings.AppSettings) {
	dialogWin := appRef.NewWindow("Save Preset")
	dialogWin.SetIcon(theme.FileIcon())

	accent := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Gold))
	accent.SetMinSize(fyne.NewSize(10, 2))
	titleLbl := widget.NewLabelWithStyle("Save Preset", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	iconW := widget.NewIcon(theme.FileIcon())
	header := container.NewHBox(iconW, widget.NewLabel(" "), titleLbl)
	headerBg := canvas.NewRectangle(ui.ColorToNRGBA(ui.CurrentTheme.Overlay))
	headerStack := container.NewStack(headerBg, container.NewPadded(header))

	nameEntry := widget.NewEntry()
	nameEntry.SetPlaceHolder("Enter preset name")

	errorLbl := widget.NewLabel("")
	errorLbl.Hide()

	saveBtn := widget.NewButton("Save", func() {
		name := strings.TrimSpace(nameEntry.Text)
		if name == "" {
			errorLbl.SetText("Name cannot be empty")
			errorLbl.Show()
			return
		}
		if settings.Contains(appSettings.Presets, name) {
			errorLbl.SetText("Preset with this name already exists")
			errorLbl.Show()
			return
		}
		if err := appSettings.SavePreset(name, appRef); err != nil {
			errorLbl.SetText("Failed to save: " + err.Error())
			errorLbl.Show()
			logsContainer.AppendLog("[ERR] Failed to save preset: " + err.Error())
			return
		}
		logsContainer.AppendLog("Saved preset: " + name)
		appRef.SendNotification(fyne.NewNotification("Preset Saved", name))
		dialogWin.Close()
	})

	cancelBtn := widget.NewButton("Cancel", func() {
		dialogWin.Close()
	})

	body := container.NewVBox(
		widget.NewLabel("Preset name:"),
		nameEntry,
		errorLbl,
	)
	bottom := container.NewGridWithColumns(2, saveBtn, cancelBtn)

	content := container.NewBorder(container.NewVBox(accent, headerStack), bottom, nil, nil, body)
	dialogWin.SetContent(content)
	dialogWin.Resize(fyne.NewSize(400, 200))
	dialogWin.CenterOnScreen()
	dialogWin.Show()
}
