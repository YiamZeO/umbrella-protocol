package settings

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"umbrella_client/internal/storage"
	"umbrella_client/internal/ui"

	"fyne.io/fyne/v2"
)

func Contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func RemoveString(slice []string, item string) []string {
	result := []string{}
	for _, s := range slice {
		if s != item {
			result = append(result, s)
		}
	}
	return result
}

// AppSettings stores persistent preferences saved to disk.
type AppSettings struct {
	LogsFontSize      float32  `json:"logsFontSize"`
	UiFontSize        float32  `json:"uiFontSize"`
	UiFontName        string   `json:"uiFontName"`
	Theme             string   `json:"theme"`
	WindowWidth       float32  `json:"windowWidth"`
	WindowHeight      float32  `json:"windowHeight"`
	KeepAliveInterval int      `json:"keepAliveInterval"`
	Timer             int      `json:"timer"`
	Presets           []string `json:"presets"`
	CurrentPreset     string   `json:"currentPreset"`
	TunnelCorePath    string   `json:"tunnelCorePath"`
	TunnelCoreArgs    string   `json:"tunnelCoreArgs"`
	AppFilesDir       string   `json:"-"`
}

func settingsFilePath(appFilesDir string) string {
	if appFilesDir == "" {
		return "settings.json"
	}
	return filepath.Join(appFilesDir, "settings.json")
}

func LoadAppSettings(appFilesDir string, appRef fyne.App) *AppSettings {
	var logTextSize float32 = 16
	// sensible defaults
	s := AppSettings{
		LogsFontSize:      logTextSize,
		UiFontSize:        0,
		UiFontName:        "",
		Theme:             ui.CurrentTheme.Name,
		WindowWidth:       400,
		WindowHeight:      600,
		KeepAliveInterval: 0,
		AppFilesDir:       appFilesDir,
	}

	// Prefer Fyne storage (works on Android) when available
	if appRef != nil && appRef.Storage() != nil {
		if rc, err := appRef.Storage().Open("settings.json"); err == nil {
			defer rc.Close()
			if b, err := io.ReadAll(rc); err == nil {
				_ = json.Unmarshal(b, &s)
				if s.LogsFontSize <= 0 {
					s.LogsFontSize = logTextSize
				}
				if s.UiFontSize < 0 {
					s.UiFontSize = 0
				}
				if s.WindowWidth <= 0 {
					s.WindowWidth = 400
				}
				if s.WindowHeight <= 0 {
					s.WindowHeight = 600
				}
				return &s
			}
		}
	}

	// Fallback to filesystem path
	p := settingsFilePath(appFilesDir)
	b, err := os.ReadFile(p)
	if err != nil {
		return &s
	}
	_ = json.Unmarshal(b, &s)
	// keep defaults if zero
	if s.LogsFontSize <= 0 {
		s.LogsFontSize = logTextSize
	}
	if s.UiFontSize < 0 {
		s.UiFontSize = 0
	}
	if s.WindowWidth <= 0 {
		s.WindowWidth = 400
	}
	if s.WindowHeight <= 0 {
		s.WindowHeight = 600
	}
	return &s
}

func SaveAppSettings(s *AppSettings, appRef fyne.App) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	var storageErr error
	var storageOK bool

	if appRef != nil && appRef.Storage() != nil {
		if w, err := appRef.Storage().Save("settings.json"); err == nil {
			if w != nil {
				_, err = w.Write(b)
				_ = w.Close()
				if err != nil {
					storageErr = err
				} else {
					storageOK = true
				}
			}
		} else {
			storageErr = err
		}
	}

	if s.AppFilesDir != "" {
		_ = os.MkdirAll(s.AppFilesDir, 0o755)
	}
	if fileErr := os.WriteFile(settingsFilePath(s.AppFilesDir), b, 0o644); fileErr != nil {
		if !storageOK {
			if storageErr != nil {
				return fmt.Errorf("storage: %v; file: %v", storageErr, fileErr)
			}
			return fileErr
		}
	}

	return nil
}

func presetConfigPath(name string, appFilesDir string) string {
	if appFilesDir == "" {
		return name + "_config.yaml"
	}
	return filepath.Join(appFilesDir, name+"_config.yaml")
}

func presetPhasesPath(name string, appFilesDir string) string {
	if appFilesDir == "" {
		return name + "_phases.yml"
	}
	return filepath.Join(appFilesDir, name+"_phases.yml")
}

func (s *AppSettings) SavePreset(name string, appRef fyne.App) error {
	configData, err := os.ReadFile(storage.ConfigFilePath(s.AppFilesDir))
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}
	if err := os.WriteFile(presetConfigPath(name, s.AppFilesDir), configData, 0o644); err != nil {
		return fmt.Errorf("failed to save preset config: %w", err)
	}

	phasesData, err := os.ReadFile(storage.PhasesFilePath(s.AppFilesDir))
	if err != nil {
		return fmt.Errorf("failed to read phases: %w", err)
	}
	if err := os.WriteFile(presetPhasesPath(name, s.AppFilesDir), phasesData, 0o644); err != nil {
		return fmt.Errorf("failed to save preset phases: %w", err)
	}

	if !Contains(s.Presets, name) {
		s.Presets = append(s.Presets, name)
	}
	s.CurrentPreset = name
	return SaveAppSettings(s, appRef)
}

func (s *AppSettings) DeletePreset(name string, appRef fyne.App) error {
	_ = os.Remove(presetConfigPath(name, s.AppFilesDir))
	_ = os.Remove(presetPhasesPath(name, s.AppFilesDir))

	s.Presets = RemoveString(s.Presets, name)
	if s.CurrentPreset == name {
		s.CurrentPreset = ""
	}
	return SaveAppSettings(s, appRef)
}

func (s *AppSettings) LoadPreset(name string, appRef fyne.App) error {
	if !Contains(s.Presets, name) {
		return fmt.Errorf("preset not found: %s", name)
	}

	configData, err := os.ReadFile(presetConfigPath(name, s.AppFilesDir))
	if err != nil {
		return fmt.Errorf("failed to read preset config: %w", err)
	}
	if err := storage.SaveConfig(configData, s.AppFilesDir, appRef); err != nil {
		return fmt.Errorf("failed to load preset config: %w", err)
	}

	phasesData, err := os.ReadFile(presetPhasesPath(name, s.AppFilesDir))
	if err != nil {
		return fmt.Errorf("failed to read preset phases: %w", err)
	}
	if err := storage.SavePhases(phasesData, s.AppFilesDir, appRef); err != nil {
		return fmt.Errorf("failed to load preset phases: %w", err)
	}

	s.CurrentPreset = name
	return SaveAppSettings(s, appRef)
}

func (s *AppSettings) ListPresets() []string {
	return s.Presets
}
