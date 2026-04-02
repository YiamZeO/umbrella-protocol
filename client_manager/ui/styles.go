package ui

import (
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
)

// Global Theme Colors
var (
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
)

// Styles
var (
	AppStyle               lipgloss.Style
	TitleStyle             lipgloss.Style
	InfoBoxStyle           lipgloss.Style
	ViewportStyle          lipgloss.Style
	StatusStyle            lipgloss.Style
	ErrorStyle             lipgloss.Style
	KeyStyle               lipgloss.Style
	DescStyle              lipgloss.Style
	ActiveConfigLabelStyle lipgloss.Style
	LogErrStyle            lipgloss.Style
)

func init() {
	RefreshStyles()
}

func RefreshStyles() {
	// Update colors from CurrentTheme
	Base = CurrentTheme.Base
	Surface = CurrentTheme.Surface
	Overlay = CurrentTheme.Overlay
	Muted = CurrentTheme.Muted
	Subtle = CurrentTheme.Subtle
	Text = CurrentTheme.Text
	Love = CurrentTheme.Love
	Gold = CurrentTheme.Gold
	Rose = CurrentTheme.Rose
	Pine = CurrentTheme.Pine
	Foam = CurrentTheme.Foam
	Iris = CurrentTheme.Iris
	Highlight = CurrentTheme.Highlight
	HighlightHigh = CurrentTheme.HighlightHigh

	// Update Styles
	AppStyle = lipgloss.NewStyle().Padding(1, 2)

	TitleStyle = lipgloss.NewStyle().
			Foreground(Base).
			Background(Pine).
			Padding(0, 1).
			Bold(true).
			MarginBottom(1)

	InfoBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Pine).
			Padding(0, 1).
			MarginBottom(1)

	ViewportStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Muted).
			Padding(0, 1)

	StatusStyle = lipgloss.NewStyle().
			Foreground(Muted)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(Love)

	KeyStyle = lipgloss.NewStyle().
			Foreground(Pine).
			Bold(true)

	DescStyle = lipgloss.NewStyle().
			Foreground(Subtle)

	ActiveConfigLabelStyle = lipgloss.NewStyle().
				Foreground(Gold).
				Bold(true)

	LogErrStyle = lipgloss.NewStyle().
			Foreground(Gold)
}

func NewListDelegate() list.DefaultDelegate {
	d := list.NewDefaultDelegate()

	// Selected items
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.
		Foreground(Gold).
		UnsetBackground().
		BorderLeftForeground(Gold)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.
		Foreground(Gold).
		UnsetBackground().
		BorderLeftForeground(Gold)

	// Normal items
	d.Styles.NormalTitle = d.Styles.NormalTitle.Foreground(Gold)
	d.Styles.NormalDesc = d.Styles.NormalDesc.Foreground(Subtle)

	// Dim the cursor a bit or keep it consistent
	d.Styles.DimmedTitle = d.Styles.DimmedTitle.Foreground(Muted)
	d.Styles.DimmedDesc = d.Styles.DimmedDesc.Foreground(Muted)

	return d
}
