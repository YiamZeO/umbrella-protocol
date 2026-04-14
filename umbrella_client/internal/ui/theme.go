package ui

import "github.com/charmbracelet/lipgloss"

// Theme defines the color palette for the application
type Theme struct {
	Name          string
	Base          lipgloss.Color
	Surface       lipgloss.Color
	Overlay       lipgloss.Color
	Muted         lipgloss.Color
	Subtle        lipgloss.Color
	Text          lipgloss.Color
	Love          lipgloss.Color
	Gold          lipgloss.Color
	Rose          lipgloss.Color
	Pine          lipgloss.Color
	Foam          lipgloss.Color
	Iris          lipgloss.Color
	Highlight     lipgloss.Color
	HighlightHigh lipgloss.Color
}

// Rosé Pine Dawn (Light) Palette
const (
	dawnBase      = "#faf4ed"
	dawnSurface   = "#fffaf3"
	dawnOverlay   = "#f2e9e1"
	dawnMuted     = "#9893a5"
	dawnSubtle    = "#797593"
	dawnText      = "#575279"
	dawnLove      = "#b4637a"
	dawnGold      = "#ea9d34"
	dawnRose      = "#d7827e"
	dawnPine      = "#286983"
	dawnFoam      = "#56949f"
	dawnIris      = "#907aa9"
	dawnHighlight = "#cecacd"
)

// Rosé Pine Moon Palette
const (
	moonBase      = "#232136"
	moonSurface   = "#2a273f"
	moonOverlay   = "#393552"
	moonMuted     = "#6e6a86"
	moonSubtle    = "#908caa"
	moonText      = "#e0def4"
	moonLove      = "#eb6f92"
	moonGold      = "#f6c177"
	moonRose      = "#ea9a97"
	moonPine      = "#3e8fb0"
	moonFoam      = "#9ccfd8"
	moonIris      = "#c4a7e7"
	moonHighlight = "#56526e"
)

// Catppuccin Latte (Light) Palette
const (
	latteBase      = "#eff1f5"
	latteSurface   = "#e6e9ef"
	latteOverlay   = "#e6e9ef"
	latteMuted     = "#a6adc8"
	latteSubtle    = "#9ca0b0"
	latteText      = "#4c4f69"
	latteLove      = "#d20f39"
	latteGold      = "#df8e1d"
	latteRose      = "#dd7878"
	lattePine      = "#1e66f5"
	latteFoam      = "#04a5e5"
	latteIris      = "#8839ef"
	latteHighlight = "#ccd0da"
)

// Catppuccin Mocha (Dark) Palette
const (
	mochaBase      = "#1e1e2e"
	mochaSurface   = "#313244"
	mochaOverlay   = "#181825"
	mochaMuted     = "#7f849c"
	mochaSubtle    = "#9399b2"
	mochaText      = "#cdd6f4"
	mochaLove      = "#f38ba8"
	mochaGold      = "#f9e2af"
	mochaRose      = "#f2cdcd"
	mochaPine      = "#89b4fa"
	mochaFoam      = "#89dceb"
	mochaIris      = "#cba6f7"
	mochaHighlight = "#45475a"
)

// Catppuccin Frappé (Darkish) Palette
const (
	frappeBase      = "#303446"
	frappeSurface   = "#414559"
	frappeOverlay   = "#292c3c"
	frappeMuted     = "#838ba7"
	frappeSubtle    = "#949cbb"
	frappeText      = "#c6d0f5"
	frappeLove      = "#e78284"
	frappeGold      = "#e5c890"
	frappeRose      = "#eebebe"
	frappePine      = "#8caaee"
	frappeFoam      = "#91d7e3"
	frappeIris      = "#ca9ee6"
	frappeHighlight = "#51576d"
)

// Catppuccin Macchiato (Darker) Palette
const (
	macchiatoBase      = "#24273a"
	macchiatoSurface   = "#363a4f"
	macchiatoOverlay   = "#1e2030"
	macchiatoMuted     = "#8087a2"
	macchiatoSubtle    = "#939ab7"
	macchiatoText      = "#cad3f5"
	macchiatoLove      = "#ed8796"
	macchiatoGold      = "#eed49f"
	macchiatoRose      = "#f0c1c1"
	macchiatoPine      = "#8aadf4"
	macchiatoFoam      = "#91d7e3"
	macchiatoIris      = "#c6a0f6"
	macchiatoHighlight = "#494d64"
)

// Predefined Themes
var (
	RosePineDawn = Theme{
		Name:          "Rose Pine Dawn",
		Base:          lipgloss.Color(dawnBase),
		Surface:       lipgloss.Color(dawnSurface),
		Overlay:       lipgloss.Color(dawnOverlay),
		Muted:         lipgloss.Color(dawnMuted),
		Subtle:        lipgloss.Color(dawnSubtle),
		Text:          lipgloss.Color(dawnText),
		Love:          lipgloss.Color(dawnLove),
		Gold:          lipgloss.Color(dawnGold),
		Rose:          lipgloss.Color(dawnRose),
		Pine:          lipgloss.Color(dawnPine),
		Foam:          lipgloss.Color(dawnFoam),
		Iris:          lipgloss.Color(dawnIris),
		Highlight:     lipgloss.Color(dawnHighlight),
		HighlightHigh: lipgloss.Color(dawnGold),
	}

	RosePineMoon = Theme{
		Name:          "Rose Pine Moon",
		Base:          lipgloss.Color(moonBase),
		Surface:       lipgloss.Color(moonSurface),
		Overlay:       lipgloss.Color(moonOverlay),
		Muted:         lipgloss.Color(moonMuted),
		Subtle:        lipgloss.Color(moonSubtle),
		Text:          lipgloss.Color(moonText),
		Love:          lipgloss.Color(moonLove),
		Gold:          lipgloss.Color(moonGold),
		Rose:          lipgloss.Color(moonRose),
		Pine:          lipgloss.Color(moonPine),
		Foam:          lipgloss.Color(moonFoam),
		Iris:          lipgloss.Color(moonIris),
		Highlight:     lipgloss.Color(moonHighlight),
		HighlightHigh: lipgloss.Color(moonGold),
	}

	CatppuccinLatte = Theme{
		Name:          "Catppuccin Latte",
		Base:          lipgloss.Color(latteBase),
		Surface:       lipgloss.Color(latteSurface),
		Overlay:       lipgloss.Color(latteOverlay),
		Muted:         lipgloss.Color(latteMuted),
		Subtle:        lipgloss.Color(latteSubtle),
		Text:          lipgloss.Color(latteText),
		Love:          lipgloss.Color(latteLove),
		Gold:          lipgloss.Color(latteGold),
		Rose:          lipgloss.Color(latteRose),
		Pine:          lipgloss.Color(lattePine),
		Foam:          lipgloss.Color(latteFoam),
		Iris:          lipgloss.Color(latteIris),
		Highlight:     lipgloss.Color(latteHighlight),
		HighlightHigh: lipgloss.Color(latteGold),
	}

	CatppuccinFrappe = Theme{
		Name:          "Catppuccin Frappe",
		Base:          lipgloss.Color(frappeBase),
		Surface:       lipgloss.Color(frappeSurface),
		Overlay:       lipgloss.Color(frappeOverlay),
		Muted:         lipgloss.Color(frappeMuted),
		Subtle:        lipgloss.Color(frappeSubtle),
		Text:          lipgloss.Color(frappeText),
		Love:          lipgloss.Color(frappeLove),
		Gold:          lipgloss.Color(frappeGold),
		Rose:          lipgloss.Color(frappeRose),
		Pine:          lipgloss.Color(frappePine),
		Foam:          lipgloss.Color(frappeFoam),
		Iris:          lipgloss.Color(frappeIris),
		Highlight:     lipgloss.Color(frappeHighlight),
		HighlightHigh: lipgloss.Color(frappeGold),
	}

	CatppuccinMacchiato = Theme{
		Name:          "Catppuccin Macchiato",
		Base:          lipgloss.Color(macchiatoBase),
		Surface:       lipgloss.Color(macchiatoSurface),
		Overlay:       lipgloss.Color(macchiatoOverlay),
		Muted:         lipgloss.Color(macchiatoMuted),
		Subtle:        lipgloss.Color(macchiatoSubtle),
		Text:          lipgloss.Color(macchiatoText),
		Love:          lipgloss.Color(macchiatoLove),
		Gold:          lipgloss.Color(macchiatoGold),
		Rose:          lipgloss.Color(macchiatoRose),
		Pine:          lipgloss.Color(macchiatoPine),
		Foam:          lipgloss.Color(macchiatoFoam),
		Iris:          lipgloss.Color(macchiatoIris),
		Highlight:     lipgloss.Color(macchiatoHighlight),
		HighlightHigh: lipgloss.Color(macchiatoGold),
	}

	CatppuccinMocha = Theme{
		Name:          "Catppuccin Mocha",
		Base:          lipgloss.Color(mochaBase),
		Surface:       lipgloss.Color(mochaSurface),
		Overlay:       lipgloss.Color(mochaOverlay),
		Muted:         lipgloss.Color(mochaMuted),
		Subtle:        lipgloss.Color(mochaSubtle),
		Text:          lipgloss.Color(mochaText),
		Love:          lipgloss.Color(mochaLove),
		Gold:          lipgloss.Color(mochaGold),
		Rose:          lipgloss.Color(mochaRose),
		Pine:          lipgloss.Color(mochaPine),
		Foam:          lipgloss.Color(mochaFoam),
		Iris:          lipgloss.Color(mochaIris),
		Highlight:     lipgloss.Color(mochaHighlight),
		HighlightHigh: lipgloss.Color(mochaGold),
	}
)

var AvailableThemes = []Theme{
	CatppuccinMocha,
	CatppuccinMacchiato,
	CatppuccinFrappe,
	CatppuccinLatte,
	RosePineMoon,
	RosePineDawn,
}

// Current Theme - Default to Mocha
var CurrentTheme = RosePineMoon

func SetThemeByName(name string) {
	for _, t := range AvailableThemes {
		if t.Name == name {
			CurrentTheme = t
			return
		}
	}
}
