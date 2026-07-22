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
	Agent   Agent
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

type Agent struct {
	MaxTextFileSize     uint64
	MaxBinaryFileSize   uint64
	MaxSessionTotalSize uint64
	MaxUploadDuration   time.Duration
}

type GC struct {
	OrphanGracePeriod time.Duration
}

type rawConfig struct {
	Storage rawStorage `toml:"storage"`
	FUSE    rawFUSE    `toml:"fuse"`
	Control rawControl `toml:"control"`
	Agent   rawAgent   `toml:"agent"`
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

type rawAgent struct {
	MaxTextFileSize     string `toml:"max_text_file_size"`
	MaxBinaryFileSize   string `toml:"max_binary_file_size"`
	MaxSessionTotalSize string `toml:"max_session_total_size"`
	MaxUploadDuration   string `toml:"max_upload_duration"`
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
		Agent: rawAgent{
			MaxTextFileSize:     "4MiB",
			MaxBinaryFileSize:   "32MiB",
			MaxSessionTotalSize: "256MiB",
			MaxUploadDuration:   "120s",
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
