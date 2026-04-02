package ui

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"client_manager/config"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/reflow/wordwrap"
)

// --- Types ---

type state int

const (
	stateMainMenu state = iota
	stateConfigMenu
	stateThemeMenu
	stateAskPath
	stateCreateConfigName
	stateCreateConfigFlags
	stateRunning
)

type listItem struct {
	title string
	desc  string
	id    string
}

func (i listItem) Title() string       { return i.title }
func (i listItem) Description() string { return i.desc }
func (i listItem) FilterValue() string { return i.title }

type MainModel struct {
	Settings *config.Settings
	state    state
	list     list.Model
	input    textinput.Model
	viewport viewport.Model
	err      error

	// For creating new config
	newConfigName string

	// For running client
	clientCmd       *exec.Cmd
	manuallyStopped bool
	logLines        []string
	logChan         chan string
	terminalWidth   int
	terminalHeight  int
}

func InitialModel(settings *config.Settings) MainModel {
	if settings.ThemeName != "" {
		SetThemeByName(settings.ThemeName)
	}

	ti := textinput.New()
	ti.Placeholder = "Введите путь..."
	ti.Focus()

	vp := viewport.New(0, 0)
	vp.Style = ViewportStyle

	m := MainModel{
		Settings: settings,
		input:    ti,
		viewport: vp,
		logChan:  make(chan string, 100),
	}

	if settings.ClientPath == "" || !config.CheckFileExists(settings.ClientPath) {
		m.state = stateAskPath
		m.input.Placeholder = "Введите путь к клиенту..."
	} else {
		m.setupMainMenu()
	}

	return m
}

func (m *MainModel) setupMainMenu() {
	m.state = stateMainMenu
}

func (m *MainModel) setupConfigMenu() {
	m.state = stateConfigMenu
	var items []list.Item
	for i, cfg := range m.Settings.Configs {
		prefix := ""
		if i == m.Settings.SelectedConfig {
			prefix = "[АКТИВНО] "
		}
		items = append(items, listItem{
			id:    fmt.Sprintf("cfg_%d", i),
			title: prefix + cfg.Name,
			desc:  cfg.Flags,
		})
	}

	d := NewListDelegate()
	m.list = list.New(items, d, 0, 0)
	m.list.Title = "Настройка конфигураций"
	m.list.SetShowHelp(false)
	m.list.Styles.Title = TitleStyle
	m.list.SetSize(m.terminalWidth-4, m.terminalHeight-15)
}

func (m *MainModel) setupThemeMenu() {
	m.state = stateThemeMenu
	var items []list.Item
	for _, t := range AvailableThemes {
		prefix := ""
		if t.Name == CurrentTheme.Name {
			prefix = "[ТЕКУЩАЯ] "
		}
		items = append(items, listItem{
			id:    t.Name,
			title: prefix + t.Name,
			desc:  "",
		})
	}

	d := NewListDelegate()
	m.list = list.New(items, d, 0, 0)
	m.list.Title = "Выбор темы оформления"
	m.list.SetShowHelp(false)
	m.list.Styles.Title = TitleStyle
	m.list.SetSize(m.terminalWidth-4, m.terminalHeight-15)

	for i, item := range m.list.Items() {
		if item.(listItem).id == CurrentTheme.Name {
			m.list.Select(i)
			break
		}
	}
}

func (m *MainModel) deleteHoveredConfig() {
	idx := m.list.Index()
	if idx >= 0 && idx < len(m.Settings.Configs) {
		m.Settings.Configs = append(m.Settings.Configs[:idx], m.Settings.Configs[idx+1:]...)
		if m.Settings.SelectedConfig == idx {
			m.Settings.SelectedConfig = -1
		} else if m.Settings.SelectedConfig > idx {
			m.Settings.SelectedConfig--
		}
		config.SaveSettings(m.Settings)
		m.setupConfigMenu()
	}
}

func (m MainModel) Init() tea.Cmd {
	return textinput.Blink
}

type logLineMsg string

func waitForLog(c chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-c
		if !ok {
			return nil
		}
		return logLineMsg(line)
	}
}

func (m MainModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.terminalWidth = msg.Width
		m.terminalHeight = msg.Height
		if m.state == stateConfigMenu || m.state == stateThemeMenu {
			m.list.SetSize(msg.Width-4, msg.Height-15)
		}
		if m.state == stateRunning {
			m.viewport.Width = msg.Width - 6
			m.viewport.Height = msg.Height - 15
		}

	case logLineMsg:
		wrappedLine := wordwrap.String(string(msg), m.viewport.Width-2)
		m.logLines = append(m.logLines, wrappedLine)
		m.viewport.SetContent(strings.Join(m.logLines, "\n"))
		m.viewport.GotoBottom()
		return m, waitForLog(m.logChan)

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		}

		switch m.state {
		case stateAskPath:
			if msg.String() == "enter" {
				path := strings.Trim(m.input.Value(), "\"")
				path = filepath.Clean(path)
				if config.CheckFileExists(path) {
					m.Settings.ClientPath = path
					config.SaveSettings(m.Settings)
					m.setupMainMenu()
					return m, nil
				} else {
					m.err = fmt.Errorf("файл не найден: %s", path)
					m.input.SetValue("")
					return m, nil
				}
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateCreateConfigName:
			if msg.String() == "enter" {
				m.newConfigName = m.input.Value()
				m.state = stateCreateConfigFlags
				m.input.SetValue("")
				m.input.Placeholder = "Введите флаги запуска..."
				return m, nil
			}
			if msg.String() == "esc" {
				m.setupMainMenu()
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateCreateConfigFlags:
			if msg.String() == "enter" {
				flags := m.input.Value()
				m.Settings.Configs = append(m.Settings.Configs, config.ClientConfig{Name: m.newConfigName, Flags: flags})
				if len(m.Settings.Configs) == 1 {
					m.Settings.SelectedConfig = 0
				}
				config.SaveSettings(m.Settings)
				m.setupMainMenu()
				return m, nil
			}
			if msg.String() == "esc" {
				m.state = stateCreateConfigName
				m.input.SetValue(m.newConfigName)
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateMainMenu:
			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "r":
				if m.Settings.SelectedConfig >= 0 && m.Settings.SelectedConfig < len(m.Settings.Configs) {
					m.state = stateRunning
					m.logLines = []string{}
					m.viewport.SetContent("")
					m.viewport.Width = m.terminalWidth - 6
					m.viewport.Height = m.terminalHeight - 15
					return m, tea.Batch(m.runClientCmd(), waitForLog(m.logChan))
				}
				m.err = fmt.Errorf("сначала выберите конфигурацию в настройках")
			case "s":
				m.setupConfigMenu()
			case "t":
				m.setupThemeMenu()
			}

		case stateConfigMenu:
			switch msg.String() {
			case "b", "esc":
				m.setupMainMenu()
			case "c":
				m.state = stateCreateConfigName
				m.input.SetValue("")
				m.input.Placeholder = "Введите имя конфигурации..."
			case "d":
				m.deleteHoveredConfig()
			case "t":
				m.setupThemeMenu()
			case "enter":
				idx := m.list.Index()
				if idx >= 0 && idx < len(m.Settings.Configs) {
					m.Settings.SelectedConfig = idx
					config.SaveSettings(m.Settings)
					m.setupConfigMenu()
				}
			default:
				m.list, cmd = m.list.Update(msg)
				return m, cmd
			}

		case stateThemeMenu:
			switch msg.String() {
			case "b", "esc":
				m.setupMainMenu()
			case "enter":
				i, ok := m.list.SelectedItem().(listItem)
				if ok {
					SetThemeByName(i.id)
					m.Settings.ThemeName = i.id
					config.SaveSettings(m.Settings)
					m.setupThemeMenu()
					return m, nil
				}
			default:
				m.list, cmd = m.list.Update(msg)
				return m, cmd
			}

		case stateRunning:
			if msg.String() == "enter" {
				if m.clientCmd != nil && m.clientCmd.Process != nil {
					m.manuallyStopped = true
					m.clientCmd.Process.Kill()
				}
				return m, nil
			}
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case clientFinishedMsg:
		m.setupMainMenu()
		if msg.err != nil && !m.manuallyStopped {
			m.err = msg.err
		}
		m.manuallyStopped = false
		return m, tea.ClearScreen
	}

	return m, tea.Batch(cmds...)
}

func (m MainModel) View() string {
	var s string

	// Common header for main states
	if m.state == stateMainMenu || m.state == stateConfigMenu || m.state == stateThemeMenu || m.state == stateRunning {
		s = TitleStyle.Render("Umbrella Client Manager") + "\n"

		// Info Box
		infoContent := fmt.Sprintf("%s %s\n", ActiveConfigLabelStyle.Render("Клиент:"), m.Settings.ClientPath)
		if m.Settings.SelectedConfig >= 0 && m.Settings.SelectedConfig < len(m.Settings.Configs) {
			cfg := m.Settings.Configs[m.Settings.SelectedConfig]
			infoContent += fmt.Sprintf("%s %s\n", ActiveConfigLabelStyle.Render("Конфиг:"), cfg.Name)
			infoContent += fmt.Sprintf("%s %s\n", ActiveConfigLabelStyle.Render("Флаги: "), cfg.Flags)
		} else {
			infoContent += ErrorStyle.Render("Конфигурация не выбрана") + "\n"
		}
		infoContent += fmt.Sprintf("%s %s", ActiveConfigLabelStyle.Render("Тема:  "), CurrentTheme.Name)
		
		wrappedInfo := wordwrap.String(infoContent, m.terminalWidth-10)
		s += InfoBoxStyle.Width(m.terminalWidth - 6).Render(wrappedInfo) + "\n"
	}

	switch m.state {
	case stateAskPath, stateCreateConfigName, stateCreateConfigFlags:
		s = TitleStyle.Render("Umbrella Client Manager") + "\n\n"
		s += m.input.View() + "\n\n"
		if m.err != nil {
			s += ErrorStyle.Render(m.err.Error()) + "\n"
		}
		s += StatusStyle.Render("(esc для отмены, ctrl+c для выхода)")

	case stateMainMenu:
		s += "\n" + StatusStyle.Render("Используйте горячие клавиши для действий")
		s += "\n\n" + fmt.Sprintf("%s %s  %s %s  %s %s  %s %s",
			KeyStyle.Render("r"), DescStyle.Render("запуск"),
			KeyStyle.Render("s"), DescStyle.Render("настройки"),
			KeyStyle.Render("t"), DescStyle.Render("тема"),
			KeyStyle.Render("q"), DescStyle.Render("выход"),
		)

		if m.err != nil {
			s += "\n\n" + ErrorStyle.Render("Ошибка: "+m.err.Error())
		}

	case stateConfigMenu:
		s += m.list.View()

		// Help Panel for Settings
		s += "\n\n" + fmt.Sprintf("%s %s  %s %s  %s %s  %s %s  %s %s",
			KeyStyle.Render("enter"), DescStyle.Render("выбрать"),
			KeyStyle.Render("c"), DescStyle.Render("создать"),
			KeyStyle.Render("d"), DescStyle.Render("удалить"),
			KeyStyle.Render("t"), DescStyle.Render("тема"),
			KeyStyle.Render("b"), DescStyle.Render("назад"),
		)

	case stateThemeMenu:
		s += m.list.View()

		// Help Panel for Themes
		s += "\n\n" + fmt.Sprintf("%s %s  %s %s",
			KeyStyle.Render("enter"), DescStyle.Render("применить"),
			KeyStyle.Render("b"), DescStyle.Render("назад"),
		)

	case stateRunning:
		s += "\n" + m.viewport.View()
		s += "\n\n" + fmt.Sprintf("%s %s  %s %s",
			KeyStyle.Render("стрелки/PgUp/PgDn"), DescStyle.Render("прокрутка логов"),
			KeyStyle.Render("enter"), DescStyle.Render("остановить клиент"),
		)
	}

	return AppStyle.Render(s)
}

// --- Client Execution ---

type clientFinishedMsg struct{ err error }

func (m *MainModel) runClientCmd() tea.Cmd {
	m.err = nil
	
	cfg := m.Settings.Configs[m.Settings.SelectedConfig]
	args := strings.Fields(cfg.Flags)
	fullPath, _ := filepath.Abs(m.Settings.ClientPath)

	m.clientCmd = exec.Command(fullPath, args...)
	
	stdout, _ := m.clientCmd.StdoutPipe()
	stderr, _ := m.clientCmd.StderrPipe()

	// Capture stdout
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			m.logChan <- scanner.Text()
		}
	}()

	// Capture stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			m.logChan <- LogErrStyle.Render(scanner.Text())
		}
	}()

	return func() tea.Msg {
		if err := m.clientCmd.Start(); err != nil {
			return clientFinishedMsg{err: err}
		}
		err := m.clientCmd.Wait()
		return clientFinishedMsg{err: err}
	}
}
