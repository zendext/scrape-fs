package config

import (
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type rawMode int64

func (mode *rawMode) UnmarshalText(text []byte) error {
	valueText := string(text)
	if !strings.HasPrefix(valueText, "0o") {
		return fmt.Errorf("control.socket_mode: must be a TOML octal integer such as 0o660")
	}

	value, err := strconv.ParseInt(strings.ReplaceAll(valueText[2:], "_", ""), 8, 64)
	if err != nil {
		return fmt.Errorf("control.socket_mode: invalid TOML octal integer")
	}
	*mode = rawMode(value)
	return nil
}

func (raw rawConfig) validate() (Config, error) {
	paths := []namedPath{
		{field: "storage.database_path", value: raw.Storage.DatabasePath},
		{field: "storage.overlay_store_path", value: raw.Storage.OverlayStorePath},
		{field: "fuse.mountpoint", value: raw.FUSE.Mountpoint},
		{field: "control.socket_path", value: raw.Control.SocketPath},
	}
	for _, path := range paths {
		if err := validateAbsolutePath(path.field, path.value); err != nil {
			return Config{}, err
		}
	}
	if err := validateDisjointPaths(paths); err != nil {
		return Config{}, err
	}

	entryTimeout, err := parseDuration("fuse.entry_timeout", raw.FUSE.EntryTimeout, true)
	if err != nil {
		return Config{}, err
	}
	attrTimeout, err := parseDuration("fuse.attr_timeout", raw.FUSE.AttrTimeout, true)
	if err != nil {
		return Config{}, err
	}
	negativeTimeout, err := parseDuration("fuse.negative_timeout", raw.FUSE.NegativeTimeout, true)
	if err != nil {
		return Config{}, err
	}
	orphanGracePeriod, err := parseDuration("gc.orphan_grace_period", raw.GC.OrphanGracePeriod, false)
	if err != nil {
		return Config{}, err
	}

	if raw.Control.AdministratorUID < 0 || raw.Control.AdministratorUID >= math.MaxUint32 {
		return Config{}, fmt.Errorf("control.administrator_uid: must be between 0 and %d", uint64(math.MaxUint32-1))
	}
	if raw.Control.SocketMode < 0o001 || raw.Control.SocketMode > 0o777 {
		return Config{}, fmt.Errorf("control.socket_mode: must contain only permission bits from 0o001 through 0o777")
	}
	if raw.Control.SocketMode&0o660 != 0o660 {
		return Config{}, fmt.Errorf("control.socket_mode: must grant read and write permission to owner and group")
	}

	return Config{
		Storage: Storage{
			DatabasePath:     raw.Storage.DatabasePath,
			OverlayStorePath: raw.Storage.OverlayStorePath,
		},
		FUSE: FUSE{
			Mountpoint:      raw.FUSE.Mountpoint,
			EntryTimeout:    entryTimeout,
			AttrTimeout:     attrTimeout,
			NegativeTimeout: negativeTimeout,
		},
		Control: Control{
			SocketPath:       raw.Control.SocketPath,
			SocketMode:       fs.FileMode(raw.Control.SocketMode),
			AdministratorUID: uint32(raw.Control.AdministratorUID),
		},
		GC: GC{
			OrphanGracePeriod: orphanGracePeriod,
		},
	}, nil
}

type namedPath struct {
	field string
	value string
}

func validateDisjointPaths(paths []namedPath) error {
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if pathContains(paths[i].value, paths[j].value) || pathContains(paths[j].value, paths[i].value) {
				return fmt.Errorf(
					"%s and %s: configured paths must be pairwise disjoint",
					paths[i].field,
					paths[j].field,
				)
			}
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func parseDuration(field, value string, allowZero bool) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", field, value, err)
	}
	if duration < 0 || (!allowZero && duration == 0) {
		requirement := "non-negative"
		if !allowZero {
			requirement = "positive"
		}
		return 0, fmt.Errorf("%s: must be %s", field, requirement)
	}
	return duration, nil
}
