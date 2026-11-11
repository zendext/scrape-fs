package gui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Preferences struct {
	Language string `json:"language"`
}

func (service *Service) preferencePath() string {
	if service.options.PreferencePath != "" {
		return service.options.PreferencePath
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, "scrapefs", "desktop.json")
}

func (service *Service) preferences() Preferences {
	preferences := Preferences{Language: "zh"}
	data, err := os.ReadFile(service.preferencePath())
	if err == nil && len(data) <= 4096 {
		var saved Preferences
		if json.Unmarshal(data, &saved) == nil && (saved.Language == "zh" || saved.Language == "en") {
			preferences = saved
		}
	}
	return preferences
}

func (service *Service) savePreferences(language string) error {
	if language != "zh" && language != "en" {
		return &Failure{Code: "InvalidPreference"}
	}
	path := service.preferencePath()
	if path == "" {
		return &Failure{Code: "PreferenceSaveFailed"}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".desktop-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(Preferences{Language: language}); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
