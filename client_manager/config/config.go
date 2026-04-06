package config

import (
	"encoding/json"
	"os"
)

type ClientConfig struct {
	Name  string `json:"name"`
	Flags string `json:"flags"`
}

type TunnelingTool struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Flags string `json:"flags"`
}

type Settings struct {
	ClientPath             string          `json:"client_path"`
	ProxiFyrePath          string          `json:"proxifyre_path,omitempty"`
	TunnelingTools         []TunnelingTool `json:"tunneling_tools"`
	SelectedTunnelingTool int             `json:"selected_tunneling_tool"`
	Configs                []ClientConfig  `json:"configs"`
	SelectedConfig         int             `json:"selected_config"`
	ThemeName              string          `json:"theme_name"`
}

const SettingsFile = "settings.json"

func LoadSettings() (*Settings, error) {
	data, err := os.ReadFile(SettingsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &Settings{
				Configs:                []ClientConfig{},
				SelectedConfig:         -1,
				TunnelingTools:         []TunnelingTool{},
				SelectedTunnelingTool: -1,
			}, nil
		}
		return nil, err
	}
	var settings Settings
	err = json.Unmarshal(data, &settings)
	if err != nil {
		return nil, err
	}
	if settings.Configs == nil {
		settings.Configs = []ClientConfig{}
	}
	if settings.TunnelingTools == nil {
		settings.TunnelingTools = []TunnelingTool{}
	}

	// Migration from ProxiFyrePath
	if settings.ProxiFyrePath != "" && len(settings.TunnelingTools) == 0 {
		settings.TunnelingTools = append(settings.TunnelingTools, TunnelingTool{
			Name: "ProxiFyre",
			Path: settings.ProxiFyrePath,
		})
		settings.SelectedTunnelingTool = 0
		settings.ProxiFyrePath = ""
		SaveSettings(&settings)
	}

	return &settings, nil
}

func SaveSettings(settings *Settings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(SettingsFile, data, 0644)
}

func CheckFileExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
