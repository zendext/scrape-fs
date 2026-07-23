package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const DefaultPath = "/etc/scrapefs/config.toml"

type Config struct {
	Storage Storage
	FUSE    FUSE
	Control Control
	GC      GC
}

type Storage struct {
	DatabasePath     string
	OverlayStorePath string
}

type FUSE struct {
	Mountpoint      string
	EntryTimeout    time.Duration
	AttrTimeout     time.Duration
	NegativeTimeout time.Duration
}

type Control struct {
	SocketPath       string
	SocketMode       fs.FileMode
	AdministratorUID uint32
}

type GC struct {
	OrphanGracePeriod time.Duration
}

type rawConfig struct {
	Storage rawStorage `toml:"storage"`
	FUSE    rawFUSE    `toml:"fuse"`
	Control rawControl `toml:"control"`
	GC      rawGC      `toml:"gc"`
}

type rawStorage struct {
	DatabasePath     string `toml:"database_path"`
	OverlayStorePath string `toml:"overlay_store_path"`
}

type rawFUSE struct {
	Mountpoint      string `toml:"mountpoint"`
	EntryTimeout    string `toml:"entry_timeout"`
	AttrTimeout     string `toml:"attr_timeout"`
	NegativeTimeout string `toml:"negative_timeout"`
}

type rawControl struct {
	SocketPath       string  `toml:"socket_path"`
	SocketMode       rawMode `toml:"socket_mode"`
	AdministratorUID int64   `toml:"administrator_uid"`
}

type rawGC struct {
	OrphanGracePeriod string `toml:"orphan_grace_period"`
}

func Load(path string) (Config, error) {
	if !filepath.IsAbs(path) {
		return Config{}, fmt.Errorf("config path: must be an absolute path")
	}

	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration %q: %w", path, err)
	}
	defer file.Close()

	config, err := Decode(file)
	if err != nil {
		return Config{}, fmt.Errorf("configuration %q: %w", path, err)
	}
	return config, nil
}

func Decode(reader io.Reader) (Config, error) {
	raw := defaultRawConfig()
	decoder := toml.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		var strictError *toml.StrictMissingError
		if errors.As(err, &strictError) {
			for _, decodeError := range strictError.Errors {
				key := decodeError.Key()
				if len(key) > 0 && key[0] == "agent" {
					return Config{}, fmt.Errorf(
						"decode TOML configuration: unknown legacy [agent] table; remove it because the external Agent is configured outside scrapefsd",
					)
				}
			}
			return Config{}, fmt.Errorf("decode TOML configuration: unknown fields:\n%s", strictError.String())
		}
		return Config{}, fmt.Errorf("decode TOML configuration: %w", err)
	}
	return raw.validate()
}

func defaultRawConfig() rawConfig {
	return rawConfig{
		Storage: rawStorage{
			DatabasePath:     "/var/lib/scrapefs/scrapefs.db",
			OverlayStorePath: "/var/lib/scrapefs/objects",
		},
		FUSE: rawFUSE{
			Mountpoint:      "/mnt/scrapefs",
			EntryTimeout:    "2s",
			AttrTimeout:     "2s",
			NegativeTimeout: "0s",
		},
		Control: rawControl{
			SocketPath:       "/run/scrapefs/scrapefs.sock",
			SocketMode:       rawMode(0o660),
			AdministratorUID: 0,
		},
		GC: rawGC{
			OrphanGracePeriod: "24h",
		},
	}
}

func validateAbsolutePath(field, path string) error {
	if path == "" {
		return fmt.Errorf("%s: must not be empty", field)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s: must be an absolute path", field)
	}
	if cleaned := filepath.Clean(path); cleaned != path {
		return fmt.Errorf("%s: must be lexically normalized; use %q", field, cleaned)
	}
	return nil
}
