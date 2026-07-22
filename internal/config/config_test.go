package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeDefaults(t *testing.T) {
	t.Parallel()

	got, err := Decode(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Decode defaults: %v", err)
	}

	if got.Storage.DatabasePath != "/var/lib/scrapefs/scrapefs.db" {
		t.Errorf("DatabasePath = %q", got.Storage.DatabasePath)
	}
	if got.Storage.OverlayStorePath != "/var/lib/scrapefs/objects" {
		t.Errorf("OverlayStorePath = %q", got.Storage.OverlayStorePath)
	}
	if got.FUSE.Mountpoint != "/mnt/scrapefs" {
		t.Errorf("Mountpoint = %q", got.FUSE.Mountpoint)
	}
	if got.FUSE.EntryTimeout != 2*time.Second {
		t.Errorf("EntryTimeout = %v", got.FUSE.EntryTimeout)
	}
	if got.FUSE.AttrTimeout != 2*time.Second {
		t.Errorf("AttrTimeout = %v", got.FUSE.AttrTimeout)
	}
	if got.FUSE.NegativeTimeout != 0 {
		t.Errorf("NegativeTimeout = %v", got.FUSE.NegativeTimeout)
	}
	if got.Control.SocketPath != "/run/scrapefs/scrapefs.sock" {
		t.Errorf("SocketPath = %q", got.Control.SocketPath)
	}
	if got.Control.SocketMode != fs.FileMode(0o660) {
		t.Errorf("SocketMode = %#o", got.Control.SocketMode)
	}
	if got.Control.AdministratorUID != 0 {
		t.Errorf("AdministratorUID = %d", got.Control.AdministratorUID)
	}
	if got.Agent.MaxTextFileSize != 4*1024*1024 {
		t.Errorf("MaxTextFileSize = %d", got.Agent.MaxTextFileSize)
	}
	if got.Agent.MaxBinaryFileSize != 32*1024*1024 {
		t.Errorf("MaxBinaryFileSize = %d", got.Agent.MaxBinaryFileSize)
	}
	if got.Agent.MaxSessionTotalSize != 256*1024*1024 {
		t.Errorf("MaxSessionTotalSize = %d", got.Agent.MaxSessionTotalSize)
	}
	if got.Agent.MaxUploadDuration != 120*time.Second {
		t.Errorf("MaxUploadDuration = %v", got.Agent.MaxUploadDuration)
	}
	if got.GC.OrphanGracePeriod != 24*time.Hour {
		t.Errorf("OrphanGracePeriod = %v", got.GC.OrphanGracePeriod)
	}
}

func TestDecodeOverrides(t *testing.T) {
	t.Parallel()

	const input = `
[storage]
database_path = "/srv/scrapefs/state.db"
overlay_store_path = "/srv/scrapefs-overlay"

[fuse]
mountpoint = "/srv/scrapefs-mount"
entry_timeout = "3s"
attr_timeout = "4s"
negative_timeout = "500ms"

[control]
socket_path = "/srv/scrapefs-run/control.sock"
socket_mode = 0o600
administrator_uid = 1000

[agent]
max_text_file_size = "8 MiB"
max_binary_file_size = "64MiB"
max_session_total_size = "1GiB"
max_upload_duration = "90s"

[gc]
orphan_grace_period = "48h"
`

	got, err := Decode(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Decode overrides: %v", err)
	}

	if got.Storage.DatabasePath != "/srv/scrapefs/state.db" {
		t.Errorf("DatabasePath = %q", got.Storage.DatabasePath)
	}
	if got.FUSE.NegativeTimeout != 500*time.Millisecond {
		t.Errorf("NegativeTimeout = %v", got.FUSE.NegativeTimeout)
	}
	if got.Control.SocketMode != fs.FileMode(0o600) {
		t.Errorf("SocketMode = %#o", got.Control.SocketMode)
	}
	if got.Control.AdministratorUID != 1000 {
		t.Errorf("AdministratorUID = %d", got.Control.AdministratorUID)
	}
	if got.Agent.MaxTextFileSize != 8*1024*1024 {
		t.Errorf("MaxTextFileSize = %d", got.Agent.MaxTextFileSize)
	}
	if got.Agent.MaxSessionTotalSize != 1024*1024*1024 {
		t.Errorf("MaxSessionTotalSize = %d", got.Agent.MaxSessionTotalSize)
	}
	if got.Agent.MaxUploadDuration != 90*time.Second {
		t.Errorf("MaxUploadDuration = %v", got.Agent.MaxUploadDuration)
	}
	if got.GC.OrphanGracePeriod != 48*time.Hour {
		t.Errorf("OrphanGracePeriod = %v", got.GC.OrphanGracePeriod)
	}
}

func TestDecodeRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "unknown field",
			input: "[storage]\nunknown_path = \"/tmp/value\"\n",
			want:  "unknown",
		},
		{
			name:  "malformed TOML",
			input: "[storage\n",
			want:  "TOML",
		},
		{
			name:  "negative UID",
			input: "[control]\nadministrator_uid = -1\n",
			want:  "control.administrator_uid",
		},
		{
			name:  "UID overflow",
			input: "[control]\nadministrator_uid = 4294967296\n",
			want:  "control.administrator_uid",
		},
		{
			name:  "zero mode",
			input: "[control]\nsocket_mode = 0o000\n",
			want:  "control.socket_mode",
		},
		{
			name:  "special mode bits",
			input: "[control]\nsocket_mode = 0o1660\n",
			want:  "control.socket_mode",
		},
		{
			name:  "decimal mode",
			input: "[control]\nsocket_mode = 432\n",
			want:  "control.socket_mode",
		},
		{
			name:  "hexadecimal mode",
			input: "[control]\nsocket_mode = 0x1B0\n",
			want:  "control.socket_mode",
		},
		{
			name:  "negative TTL",
			input: "[fuse]\nentry_timeout = \"-1s\"\n",
			want:  "fuse.entry_timeout",
		},
		{
			name:  "zero upload duration",
			input: "[agent]\nmax_upload_duration = \"0s\"\n",
			want:  "agent.max_upload_duration",
		},
		{
			name:  "zero grace period",
			input: "[gc]\norphan_grace_period = \"0s\"\n",
			want:  "gc.orphan_grace_period",
		},
		{
			name:  "zero size",
			input: "[agent]\nmax_text_file_size = \"0MiB\"\n",
			want:  "agent.max_text_file_size",
		},
		{
			name:  "unsupported size unit",
			input: "[agent]\nmax_text_file_size = \"4MB\"\n",
			want:  "agent.max_text_file_size",
		},
		{
			name: "text quota exceeds session quota",
			input: `
[agent]
max_text_file_size = "16MiB"
max_session_total_size = "8MiB"
`,
			want: "agent.max_text_file_size",
		},
		{
			name: "binary quota exceeds session quota",
			input: `
[agent]
max_binary_file_size = "16MiB"
max_session_total_size = "8MiB"
`,
			want: "agent.max_binary_file_size",
		},
		{
			name:  "size overflow",
			input: "[agent]\nmax_text_file_size = \"" + "18446744073709551615GiB" + "\"\n",
			want:  "agent.max_text_file_size",
		},
		{
			name:  "relative path",
			input: "[storage]\ndatabase_path = \"state.db\"\n",
			want:  "storage.database_path",
		},
		{
			name:  "unclean path",
			input: "[storage]\ndatabase_path = \"/var/lib/../lib/scrapefs.db\"\n",
			want:  "storage.database_path",
		},
		{
			name: "equal paths",
			input: `
[storage]
database_path = "/srv/scrapefs"
overlay_store_path = "/srv/scrapefs"
`,
			want: "pairwise disjoint",
		},
		{
			name: "contained path",
			input: `
[storage]
database_path = "/srv/scrapefs/state.db"
overlay_store_path = "/srv/scrapefs"
`,
			want: "pairwise disjoint",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode(strings.NewReader(tt.input))
			if err == nil {
				t.Fatal("Decode succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoadRequiresAbsolutePath(t *testing.T) {
	t.Parallel()

	_, err := Load("config.toml")
	if err == nil {
		t.Fatal("Load relative path succeeded, want error")
	}
	if !strings.Contains(err.Error(), "config path") {
		t.Errorf("Load relative path error = %q", err)
	}
}

func TestLoadAcceptsNonCleanAbsolutePath(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	cleanPath := filepath.Join(tempDir, "config.toml")
	if err := os.WriteFile(cleanPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path := tempDir + "//config.toml"
	if _, err := Load(path); err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.toml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load missing file succeeded, want error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want path %q", err, path)
	}
}

func TestDecodeRejectsSizeAboveUint64(t *testing.T) {
	t.Parallel()

	input := "[agent]\nmax_text_file_size = \"" + strings.Repeat("9", 30) + "GiB\"\n"
	_, err := Decode(strings.NewReader(input))
	if err == nil {
		t.Fatal("Decode oversized value succeeded, want error")
	}
	if !strings.Contains(err.Error(), "agent.max_text_file_size") {
		t.Errorf("error = %q", err)
	}
}

func TestLoadReadsExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[control]\nadministrator_uid = 42\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Control.AdministratorUID != 42 {
		t.Errorf("AdministratorUID = %d", got.Control.AdministratorUID)
	}
}
