package main

import (
	"fmt"
	"os"

	"client_manager/config"
	"client_manager/ui"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	settings, err := config.LoadSettings()
	if err != nil {
		fmt.Printf("Ошибка при загрузке настроек: %v\n", err)
		os.Exit(1)
	}

	p := tea.NewProgram(ui.InitialModel(settings), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Ошибка: %v", err)
		os.Exit(1)
	}
}
