package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
)

// Paths and helpers for dynamic config and phases files
func ConfigFilePath(appFilesDir string) string {
	if appFilesDir == "" {
		return "config.yaml"
	}
	return filepath.Join(appFilesDir, "config.yaml")
}

func PhasesFilePath(appFilesDir string) string {
	if appFilesDir == "" {
		return "phases.yml"
	}
	return filepath.Join(appFilesDir, "phases.yml")
}

func FontsDirPath(appFilesDir string) string {
	if appFilesDir == "" {
		return "fonts"
	}
	return filepath.Join(appFilesDir, "fonts")
}

type FontInfo struct {
	Name string
	Path string
}

func DiscoverFonts(appFilesDir string) []FontInfo {
	dir := FontsDirPath(appFilesDir)
	list := []FontInfo{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return list
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		l := strings.ToLower(n)
		if strings.HasSuffix(l, ".ttf") || strings.HasSuffix(l, ".otf") {
			list = append(list, FontInfo{Name: n, Path: filepath.Join(dir, n)})
		}
	}
	return list
}

func LoadUIFontByName(name string, appFilesDir string) ([]byte, string, error) {
	for _, f := range DiscoverFonts(appFilesDir) {
		if f.Name == name {
			b, err := os.ReadFile(f.Path)
			return b, f.Name, err
		}
	}
	p := filepath.Join(FontsDirPath(appFilesDir), name)
	b, err := os.ReadFile(p)
	return b, name, err
}

func SaveUIFontToFontsDir(name string, data []byte, appFilesDir string) error {
	if appFilesDir != "" {
		_ = os.MkdirAll(appFilesDir, 0o755)
	}
	_ = os.MkdirAll(FontsDirPath(appFilesDir), 0o755)
	p := filepath.Join(FontsDirPath(appFilesDir), name)
	return os.WriteFile(p, data, 0o644)
}

func LoadConfig(appFilesDir string, appRef fyne.App) ([]byte, error) {
	// Try Fyne storage first
	if appRef != nil && appRef.Storage() != nil {
		if rc, err := appRef.Storage().Open("config.yaml"); err == nil {
			defer rc.Close()
			if b, err := io.ReadAll(rc); err == nil {
				if len(b) > 0 {
					return b, nil
				}
				return nil, fmt.Errorf("config.yaml empty in storage")
			}
		}
	}

	// Fallback to filesystem
	p := ConfigFilePath(appFilesDir)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("config.yaml empty on disk")
	}
	return b, nil
}

func SaveConfig(b []byte, appFilesDir string, appRef fyne.App) error {
	var storageErr error
	var storageOK bool
	if appRef != nil && appRef.Storage() != nil {
		if w, err := appRef.Storage().Save("config.yaml"); err == nil {
			if w != nil {
				if _, err := w.Write(b); err != nil {
					storageErr = err
				} else {
					storageOK = true
				}
				_ = w.Close()
			}
		} else {
			storageErr = err
		}
	}
	if appFilesDir != "" {
		_ = os.MkdirAll(appFilesDir, 0o755)
	}
	if fileErr := os.WriteFile(ConfigFilePath(appFilesDir), b, 0o644); fileErr != nil {
		if !storageOK {
			if storageErr != nil {
				return fmt.Errorf("storage: %v; file: %v", storageErr, fileErr)
			}
			return fileErr
		}
	}
	return nil
}

func LoadPhases(appFilesDir string, appRef fyne.App) ([]byte, error) {
	if appRef != nil && appRef.Storage() != nil {
		if rc, err := appRef.Storage().Open("phases.yml"); err == nil {
			defer rc.Close()
			if b, err := io.ReadAll(rc); err == nil {
				if len(b) > 0 {
					return b, nil
				}
				return nil, fmt.Errorf("phases.yml empty in storage")
			}
		}
	}
	p := PhasesFilePath(appFilesDir)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("phases.yml empty on disk")
	}
	return b, nil
}

func SavePhases(b []byte, appFilesDir string, appRef fyne.App) error {
	var storageErr error
	var storageOK bool
	if appRef != nil && appRef.Storage() != nil {
		if w, err := appRef.Storage().Save("phases.yml"); err == nil {
			if w != nil {
				if _, err := w.Write(b); err != nil {
					storageErr = err
				} else {
					storageOK = true
				}
				_ = w.Close()
			}
		} else {
			storageErr = err
		}
	}
	if appFilesDir != "" {
		_ = os.MkdirAll(appFilesDir, 0o755)
	}
	if fileErr := os.WriteFile(PhasesFilePath(appFilesDir), b, 0o644); fileErr != nil {
		if !storageOK {
			if storageErr != nil {
				return fmt.Errorf("storage: %v; file: %v", storageErr, fileErr)
			}
			return fileErr
		}
	}
	return nil
}
