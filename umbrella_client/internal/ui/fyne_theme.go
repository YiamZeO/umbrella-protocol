package ui

import (
	"image/color"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	ftheme "fyne.io/fyne/v2/theme"
	"github.com/charmbracelet/lipgloss"
)

// NewFyneTheme returns a fyne.Theme that maps the current TUI Theme colors
// into Fyne theme color roles. It delegates icons, fonts and sizes to the
// default Fyne theme.
func NewFyneTheme() fyne.Theme {
	return fyneTheme{}
}

type fyneTheme struct{}

var (
	uiFontRes      fyne.Resource
	monoFontRes    fyne.Resource
	uiBaseTextSize float32
	uiFontName     string
)

func (fyneTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case ftheme.ColorNameBackground:
		return ColorToNRGBA(CurrentTheme.Base)
	case ftheme.ColorNameButton:
		return ColorToNRGBA(CurrentTheme.Surface)
	case ftheme.ColorNameDisabled:
		return ColorToNRGBA(CurrentTheme.Muted)
	case ftheme.ColorNameError:
		return ColorToNRGBA(CurrentTheme.Love)
	case ftheme.ColorNameFocus:
		return ColorToNRGBA(CurrentTheme.Highlight)
	case ftheme.ColorNameHover:
		return ColorToNRGBA(CurrentTheme.HighlightHigh)
	case ftheme.ColorNameForeground:
		return ColorToNRGBA(CurrentTheme.Text)
	default:
		return ftheme.DefaultTheme().Color(name, variant)
	}
}

func (fyneTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return ftheme.DefaultTheme().Icon(name)
}

func (fyneTheme) Font(style fyne.TextStyle) fyne.Resource {
	if style.Monospace {
		if monoFontRes != nil {
			return monoFontRes
		}
	} else {
		if uiFontRes != nil {
			return uiFontRes
		}
	}
	return ftheme.DefaultTheme().Font(style)
}

func (fyneTheme) Size(name fyne.ThemeSizeName) float32 {
	if name == ftheme.SizeNameText && uiBaseTextSize > 0 {
		return uiBaseTextSize
	}
	return ftheme.DefaultTheme().Size(name)
}

// ColorToNRGBA converts a lipgloss color (hex string) to an image/color.Color.
// Supports "#rrggbb" and "#rrggbbaa" forms. Falls back to black on parse error.
func ColorToNRGBA(h lipgloss.Color) color.Color {
	s := strings.TrimSpace(string(h))
	if strings.HasPrefix(s, "#") {
		s = s[1:]
	}
	if len(s) == 6 {
		r, _ := strconv.ParseUint(s[0:2], 16, 8)
		g, _ := strconv.ParseUint(s[2:4], 16, 8)
		b, _ := strconv.ParseUint(s[4:6], 16, 8)
		return &color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff}
	}
	if len(s) == 8 {
		r, _ := strconv.ParseUint(s[0:2], 16, 8)
		g, _ := strconv.ParseUint(s[2:4], 16, 8)
		b, _ := strconv.ParseUint(s[4:6], 16, 8)
		a, _ := strconv.ParseUint(s[6:8], 16, 8)
		return &color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: uint8(a)}
	}
	// fallback
	return &color.NRGBA{R: 0, G: 0, B: 0, A: 0xff}
}

func SetUIFontFromBytes(name string, data []byte) {
	if data == nil || len(data) == 0 {
		uiFontRes = nil
		uiFontName = ""
		return
	}
	uiFontRes = fyne.NewStaticResource(name, data)
	uiFontName = name
}

func SetUIMonoFontFromBytes(name string, data []byte) {
	if data == nil || len(data) == 0 {
		monoFontRes = nil
		return
	}
	monoFontRes = fyne.NewStaticResource(name, data)
}

func ClearUIFont() {
	uiFontRes = nil
	monoFontRes = nil
	uiFontName = ""
}

func SetUIFontSize(size float32) {
	uiBaseTextSize = size
}

func GetUIFontName() string {
	return uiFontName
}
