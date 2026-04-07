package ui

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"client_manager/config"

	"github.com/atotto/clipboard"
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
	stateTunnelingMenu
	stateThemeMenu
	stateAskPath
	stateCreateConfigName
	stateCreateConfigFlags
	stateCreateTunnelName
	stateCreateTunnelPath
	stateCreateTunnelFlags
	stateAskTimedMinutes
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

	// For creating new config/tunnel
	newItemName string
	newItemPath string

	// For running client
	clientCmd       *exec.Cmd
	manuallyStopped bool
	logLines        []string
	logChan         chan string
	terminalWidth   int
	terminalHeight  int

	// For Tunneling Tool
	tunnelCmd     *exec.Cmd
	tunnelRunning bool

	// For timed runs
	isTimedRun     bool
	returnToConfig bool
	timedConfig    config.ClientConfig
	remainingTime  int
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
	m.list.Title = "Конфигурации"
	m.list.SetShowHelp(false)
	m.list.Styles.Title = TitleStyle
	m.list.SetSize(m.terminalWidth-4, m.terminalHeight-15)
}

func (m *MainModel) setupTunnelingMenu() {
	m.state = stateTunnelingMenu
	var items []list.Item
	for i, t := range m.Settings.TunnelingTools {
		prefix := ""
		if i == m.Settings.SelectedTunnelingTool {
			prefix = "[АКТИВНО] "
		}
		items = append(items, listItem{
			id:    fmt.Sprintf("tunnel_%d", i),
			title: prefix + t.Name,
			desc:  t.Path,
		})
	}

	d := NewListDelegate()
	m.list = list.New(items, d, 0, 0)
	m.list.Title = "Средства туннелирования"
	m.list.SetShowHelp(false)
	m.list.Styles.Title = TitleStyle
	m.list.SetSize(m.terminalWidth-4, m.terminalHeight-15)
}

func (m *MainModel) deleteHoveredTunnel() {
	idx := m.list.Index()
	if idx >= 0 && idx < len(m.Settings.TunnelingTools) {
		m.Settings.TunnelingTools = append(m.Settings.TunnelingTools[:idx], m.Settings.TunnelingTools[idx+1:]...)
		if m.Settings.SelectedTunnelingTool == idx {
			m.Settings.SelectedTunnelingTool = -1
		} else if m.Settings.SelectedTunnelingTool > idx {
			m.Settings.SelectedTunnelingTool--
		}
		config.SaveSettings(m.Settings)
		m.setupTunnelingMenu()
	}
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

type tickMsg struct{}

func waitForLog(c chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-c
		if !ok {
			return nil
		}
		return logLineMsg(line)
	}
}

func (m *MainModel) tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return tickMsg{}
	})
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

	case tickMsg:
		if m.isTimedRun && m.state == stateRunning {
			m.remainingTime--
			if m.remainingTime <= 0 {
				m.remainingTime = 0
				if m.clientCmd != nil && m.clientCmd.Process != nil {
					softKill(m.clientCmd.Process.Pid)
				}
			}
			if m.remainingTime > 0 {
				return m, m.tick()
			}
		}
		return m, nil

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
				m.newItemName = m.input.Value()
				m.state = stateCreateConfigFlags
				m.input.SetValue("")
				m.input.Placeholder = "Введите флаги запуска..."
				return m, nil
			}
			if msg.String() == "esc" {
				m.setupConfigMenu()
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateCreateConfigFlags:
			if msg.String() == "enter" {
				flags := m.input.Value()
				m.Settings.Configs = append(m.Settings.Configs, config.ClientConfig{Name: m.newItemName, Flags: flags})
				if len(m.Settings.Configs) == 1 {
					m.Settings.SelectedConfig = 0
				}
				config.SaveSettings(m.Settings)
				m.setupConfigMenu()
				return m, nil
			}
			if msg.String() == "esc" {
				m.state = stateCreateConfigName
				m.input.SetValue(m.newItemName)
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateCreateTunnelName:
			if msg.String() == "enter" {
				m.newItemName = m.input.Value()
				m.state = stateCreateTunnelPath
				m.input.SetValue("")
				m.input.Placeholder = "Введите путь к средству туннелирования..."
				return m, nil
			}
			if msg.String() == "esc" {
				m.setupTunnelingMenu()
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateCreateTunnelPath:
			if msg.String() == "enter" {
				path := strings.Trim(m.input.Value(), "\"")
				path = filepath.Clean(path)
				if config.CheckFileExists(path) {
					m.newItemPath = path
					m.state = stateCreateTunnelFlags
					m.input.SetValue("")
					m.input.Placeholder = "Введите флаги запуска (необязательно)..."
					return m, nil
				} else {
					m.err = fmt.Errorf("файл не найден: %s", path)
					m.input.SetValue("")
					return m, nil
				}
			}
			if msg.String() == "esc" {
				m.state = stateCreateTunnelName
				m.input.SetValue(m.newItemName)
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateCreateTunnelFlags:
			if msg.String() == "enter" {
				flags := m.input.Value()
				m.Settings.TunnelingTools = append(m.Settings.TunnelingTools, config.TunnelingTool{
					Name:  m.newItemName,
					Path:  m.newItemPath,
					Flags: flags,
				})
				if len(m.Settings.TunnelingTools) == 1 {
					m.Settings.SelectedTunnelingTool = 0
				}
				config.SaveSettings(m.Settings)
				m.setupTunnelingMenu()
				return m, nil
			}
			if msg.String() == "esc" {
				m.state = stateCreateTunnelPath
				m.input.SetValue(m.newItemPath)
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateAskTimedMinutes:
			if msg.String() == "enter" {
				if val := strings.TrimSpace(m.input.Value()); val != "" {
					if mins, err := strconv.Atoi(val); err == nil && mins > 0 {
						m.isTimedRun = true
						m.returnToConfig = true
						m.remainingTime = mins * 60
						m.state = stateRunning
						m.logLines = []string{}
						m.viewport.SetContent("")
						m.viewport.Width = m.terminalWidth - 6
						m.viewport.Height = m.terminalHeight - 15
						return m, tea.Batch(m.runClientCmd(), m.tick(), waitForLog(m.logChan))
					} else {
						m.err = fmt.Errorf("введите положительное целое число минут")
						m.input.SetValue("")
						return m, nil
					}
				}
			}
			if msg.String() == "esc" || msg.String() == "b" {
				m.setupConfigMenu()
				return m, nil
			}
			m.input, cmd = m.input.Update(msg)
			return m, cmd

		case stateMainMenu:
			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "r":
				m.isTimedRun = false
				m.returnToConfig = false
				if m.Settings.SelectedConfig >= 0 && m.Settings.SelectedConfig < len(m.Settings.Configs) {
					m.state = stateRunning
					m.logLines = []string{}
					m.viewport.SetContent("")
					m.viewport.Width = m.terminalWidth - 6
					m.viewport.Height = m.terminalHeight - 15
					return m, tea.Batch(m.runClientCmd(), waitForLog(m.logChan))
				}
				m.err = fmt.Errorf("сначала выберите конфигурацию в настройках")
			case "e":
				return m, m.toggleTunnel()
			case "k":
				m.setupConfigMenu()
			case "s":
				m.setupTunnelingMenu()
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
			case "r":
				idx := m.list.Index()
				if idx >= 0 && idx < len(m.Settings.Configs) {
					m.timedConfig = m.Settings.Configs[idx]
					m.state = stateAskTimedMinutes
					m.input.SetValue("")
					m.input.Placeholder = "Введите количество минут..."
					m.err = nil
					return m, nil
				}
			case "d":
				m.deleteHoveredConfig()
			case "o":
				idx := m.list.Index()
				if idx >= 0 && idx < len(m.Settings.Configs) {
					cfg := m.Settings.Configs[idx]
					return m, m.copyToClipboard(cfg.Flags)
				}
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

		case stateTunnelingMenu:
			switch msg.String() {
			case "b", "esc":
				m.setupMainMenu()
			case "c":
				m.state = stateCreateTunnelName
				m.input.SetValue("")
				m.input.Placeholder = "Введите имя средства туннелирования..."
			case "d":
				m.deleteHoveredTunnel()
			case "enter":
				idx := m.list.Index()
				if idx >= 0 && idx < len(m.Settings.TunnelingTools) {
					m.Settings.SelectedTunnelingTool = idx
					config.SaveSettings(m.Settings)
					m.setupTunnelingMenu()
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
			switch msg.String() {
			case "enter":
				if m.clientCmd != nil && m.clientCmd.Process != nil {
					m.manuallyStopped = true
					softKill(m.clientCmd.Process.Pid)
				}
				m.isTimedRun = false
				return m, nil
			case "e":
				return m, m.toggleTunnel()
			}
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case clientFinishedMsg:
		return m, m.handleClientFinished(msg.err)

	case tunnelFinishedMsg:
		m.handleTunnelFinished(msg.err)
		return m, nil
	}

	return m, tea.Batch(cmds...)
}

func (m MainModel) View() string {
	var s string

	// Common header for main states
	if m.state == stateMainMenu || m.state == stateConfigMenu || m.state == stateTunnelingMenu || m.state == stateThemeMenu || m.state == stateRunning {
		s = TitleStyle.Render("Umbrella Client Manager") + "\n"

		// Info Box
		infoContent := fmt.Sprintf("%s %s\n", ActiveConfigLabelStyle.Render("Клиент:"), m.Settings.ClientPath)

		tunnelName := "не выбрано"
		tunnelStatus := "выключено"
		if m.Settings.SelectedTunnelingTool >= 0 && m.Settings.SelectedTunnelingTool < len(m.Settings.TunnelingTools) {
			t := m.Settings.TunnelingTools[m.Settings.SelectedTunnelingTool]
			tunnelName = t.Name
			if m.tunnelRunning {
				tunnelStatus = "включено"
			}
		}
		infoContent += fmt.Sprintf("%s %s: %s\n", ActiveConfigLabelStyle.Render("Туннель:"), tunnelName, tunnelStatus)

		if m.isTimedRun && m.remainingTime > 0 {
			min := m.remainingTime / 60
			sec := m.remainingTime % 60
			timerStr := fmt.Sprintf("[%dm%ds]", min, sec)
			infoContent += fmt.Sprintf("%s %s %s\n", ActiveConfigLabelStyle.Render("Конфиг:"), m.timedConfig.Name, timerStr)
		} else if m.Settings.SelectedConfig >= 0 && m.Settings.SelectedConfig < len(m.Settings.Configs) {
			cfg := m.Settings.Configs[m.Settings.SelectedConfig]
			infoContent += fmt.Sprintf("%s %s\n", ActiveConfigLabelStyle.Render("Конфиг:"), cfg.Name)
		} else {
			infoContent += ErrorStyle.Render("Конфигурация не выбрана") + "\n"
		}
		infoContent += fmt.Sprintf("%s %s", ActiveConfigLabelStyle.Render("Тема:  "), CurrentTheme.Name)

		wrappedInfo := wordwrap.String(infoContent, m.terminalWidth-10)
		s += InfoBoxStyle.Width(m.terminalWidth-6).Render(wrappedInfo) + "\n"
	}

	switch m.state {
	case stateAskPath, stateCreateConfigName, stateCreateConfigFlags, stateCreateTunnelName, stateCreateTunnelPath, stateCreateTunnelFlags, stateAskTimedMinutes:
		s = TitleStyle.Render("Umbrella Client Manager") + "\n\n"
		s += m.input.View() + "\n\n"
		if m.err != nil {
			s += ErrorStyle.Render(m.err.Error()) + "\n"
		}
		s += StatusStyle.Render("(esc для отмены, ctrl+c для выхода)")

	case stateMainMenu:
		s += "\n" + StatusStyle.Render("Используйте горячие клавиши для действий")

		tunnelAction := "включение\\выключение"
		if m.Settings.SelectedTunnelingTool >= 0 && m.Settings.SelectedTunnelingTool < len(m.Settings.TunnelingTools) {
			t := m.Settings.TunnelingTools[m.Settings.SelectedTunnelingTool]
			tunnelAction = fmt.Sprintf("включение\\выключение %s", t.Name)
		}

		s += "\n\n" + fmt.Sprintf("%s %s  %s %s  %s %s  %s %s  %s %s  %s %s",
			KeyStyle.Render("r"), DescStyle.Render("запуск"),
			KeyStyle.Render("e"), DescStyle.Render(tunnelAction),
			KeyStyle.Render("k"), DescStyle.Render("конфигурации"),
			KeyStyle.Render("s"), DescStyle.Render("средство туннелирования"),
			KeyStyle.Render("t"), DescStyle.Render("тема"),
			KeyStyle.Render("q"), DescStyle.Render("выход"),
		)

		if m.err != nil {
			s += "\n\n" + ErrorStyle.Render("Ошибка: "+m.err.Error())
		}

	case stateConfigMenu:
		s += m.list.View()

		// Help Panel for Settings
		s += "\n\n" + fmt.Sprintf("%s %s  %s %s  %s %s  %s %s  %s %s  %s %s  %s %s",
			KeyStyle.Render("enter"), DescStyle.Render("выбрать"),
			KeyStyle.Render("r"), DescStyle.Render("запустить на время"),
			KeyStyle.Render("c"), DescStyle.Render("создать"),
			KeyStyle.Render("d"), DescStyle.Render("удалить"),
			KeyStyle.Render("o"), DescStyle.Render("скопировать флаги"),
			KeyStyle.Render("t"), DescStyle.Render("тема"),
			KeyStyle.Render("b"), DescStyle.Render("назад"),
		)

	case stateTunnelingMenu:
		s += m.list.View()

		// Help Panel for Tunneling
		s += "\n\n" + fmt.Sprintf("%s %s  %s %s  %s %s  %s %s",
			KeyStyle.Render("enter"), DescStyle.Render("выбрать"),
			KeyStyle.Render("c"), DescStyle.Render("создать"),
			KeyStyle.Render("d"), DescStyle.Render("удалить"),
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

		tunnelAction := "включение\\выключение туннеля"
		if m.Settings.SelectedTunnelingTool >= 0 && m.Settings.SelectedTunnelingTool < len(m.Settings.TunnelingTools) {
			t := m.Settings.TunnelingTools[m.Settings.SelectedTunnelingTool]
			tunnelAction = fmt.Sprintf("включение\\выключение %s", t.Name)
		}

		s += "\n\n" + fmt.Sprintf("%s %s  %s %s  %s %s",
			KeyStyle.Render("стрелки/PgUp/PgDn"), DescStyle.Render("прокрутка логов"),
			KeyStyle.Render("e"), DescStyle.Render(tunnelAction),
			KeyStyle.Render("enter"), DescStyle.Render("остановить клиент"),
		)
	}

	return AppStyle.Render(s)
}

// --- Client Execution ---

type clientFinishedMsg struct{ err error }

func (m *MainModel) runClientCmd() tea.Cmd {
	m.err = nil

	var cfg config.ClientConfig
	if m.isTimedRun {
		cfg = m.timedConfig
	} else if m.Settings.SelectedConfig >= 0 && m.Settings.SelectedConfig < len(m.Settings.Configs) {
		cfg = m.Settings.Configs[m.Settings.SelectedConfig]
	} else {
		m.err = fmt.Errorf("конфигурация не выбрана")
		return func() tea.Msg {
			return clientFinishedMsg{err: fmt.Errorf("конфигурация не выбрана")}
		}
	}
	args := strings.Fields(cfg.Flags)
	fullPath, _ := filepath.Abs(m.Settings.ClientPath)

	m.clientCmd = exec.Command(fullPath, args...)
	setProcessGroup(m.clientCmd)

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

// --- Tunneling Execution ---

type tunnelFinishedMsg struct{ err error }

func (m *MainModel) toggleTunnel() tea.Cmd {
	if m.tunnelRunning {
		m.stopTunnel()
		return nil
	}

	if m.Settings.SelectedTunnelingTool < 0 || m.Settings.SelectedTunnelingTool >= len(m.Settings.TunnelingTools) {
		m.err = fmt.Errorf("сначала выберите средство туннелирования в настройках")
		return nil
	}

	return m.startTunnel()
}

func (m *MainModel) startTunnel() tea.Cmd {
	t := m.Settings.TunnelingTools[m.Settings.SelectedTunnelingTool]
	fullPath, _ := filepath.Abs(t.Path)
	dir := filepath.Dir(fullPath)
	args := strings.Fields(t.Flags)

	m.tunnelCmd = exec.Command(fullPath, args...)
	m.tunnelCmd.Dir = dir
	setProcessGroup(m.tunnelCmd)

	err := m.tunnelCmd.Start()
	if err != nil {
		m.err = fmt.Errorf("ошибка запуска %s: %v", t.Name, err)
		return nil
	}

	m.tunnelRunning = true

	return func() tea.Msg {
		err := m.tunnelCmd.Wait()
		return tunnelFinishedMsg{err: err}
	}
}

func (m *MainModel) stopTunnel() {
	if m.tunnelCmd != nil && m.tunnelCmd.Process != nil {
		softKill(m.tunnelCmd.Process.Pid)
	}
}

func (m *MainModel) handleTunnelFinished(err error) {
	m.tunnelRunning = false
	if err != nil && !isGracefulExit(err) {
		tName := "средство туннелирования"
		if m.Settings.SelectedTunnelingTool >= 0 && m.Settings.SelectedTunnelingTool < len(m.Settings.TunnelingTools) {
			tName = m.Settings.TunnelingTools[m.Settings.SelectedTunnelingTool].Name
		}
		m.err = fmt.Errorf("%s завершилось с ошибкой: %v", tName, err)
	}
}

func (m *MainModel) handleClientFinished(err error) tea.Cmd {
	if m.returnToConfig {
		m.setupConfigMenu()
		m.returnToConfig = false
	} else {
		m.setupMainMenu()
	}
	m.isTimedRun = false
	m.remainingTime = 0
	m.timedConfig = config.ClientConfig{}
	if err != nil && !m.manuallyStopped && !isGracefulExit(err) {
		m.err = err
	}
	m.manuallyStopped = false
	return tea.ClearScreen
}

func (m *MainModel) copyToClipboard(text string) tea.Cmd {
	err := clipboard.WriteAll(text)
	if err != nil {
		m.err = fmt.Errorf("ошибка копирования в буфер обмена: %v", err)
	}
	return nil
}
