package gui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/control"
)

func TestConfigurationPreviewPreservesCommentsAndRejectsChangedDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# administrator note\n[fuse]\nattr_timeout = '2s' # keep\n"
	os.WriteFile(path, []byte(original), 0o600)
	service := New(Options{ConfigPath: path})
	document, _ := config.ParseDocument(original)
	preview, err := service.previewConfiguration(Request{Digest: document.Digest, Fields: map[string]string{"fuse.attr_timeout": "4s"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.Document.Text, "# administrator note") || !strings.Contains(preview.Document.Text, "# keep") || len(preview.Changes) != 1 || preview.StorageChanged {
		t.Fatalf("preview=%+v", preview)
	}
	current, _ := os.ReadFile(path)
	if string(current) != original {
		t.Fatal("preview wrote disk configuration")
	}
	os.WriteFile(path, []byte(original+"# external edit\n"), 0o600)
	if _, err := service.previewConfiguration(Request{Digest: document.Digest, Raw: true, Text: preview.Document.Text}); failure(err).Code != "ConfigurationChanged" {
		t.Fatalf("changed disk accepted: %v", err)
	}
}

func TestRawConfigurationCanRepairInvalidDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[invalid"), 0o600)
	service := New(Options{ConfigPath: path})
	document, err := readConfiguration(path)
	if err != nil || document.ValidationError == "" || document.Digest == "" {
		t.Fatalf("document=%+v, %v", document, err)
	}
	preview, err := service.previewConfiguration(Request{Digest: document.Digest, Raw: true, Text: "# repaired\n"})
	if err != nil || preview.Document.ValidationError != "" {
		t.Fatalf("repair=%+v %v", preview, err)
	}
}

func TestCustomEndpointRejectsSystemActionsBeforeAuthorization(t *testing.T) {
	service := New(Options{ConfigPath: filepath.Join(t.TempDir(), "custom.toml"), SocketPath: "/tmp/isolated.sock"})
	for _, command := range []string{"service", "save_configuration", "authorize_configuration"} {
		response := service.Execute(context.Background(), Request{Command: command, Action: "restart"})
		if response.Error == nil || response.Error.Code != "SystemUnavailable" {
			t.Fatalf("%s=%+v", command, response)
		}
	}
}

func TestServiceActionAllowlistAndPreferencePersistence(t *testing.T) {
	calls := 0
	path := filepath.Join(t.TempDir(), "gui", "desktop.json")
	options := Options{PreferencePath: path, Admin: func(context.Context, []string, any) (json.RawMessage, error) { calls++; return nil, nil }}
	service := New(options)
	if err := service.serviceAction(context.Background(), "enable"); failure(err).Code != "UnsupportedOperation" || calls != 0 {
		t.Fatalf("unexpected admin call: %v / %d", err, calls)
	}
	if err := service.savePreferences("en"); err != nil {
		t.Fatal(err)
	}
	if New(options).preferences().Language != "en" || calls != 0 {
		t.Fatal("preference was not restored independently of system management")
	}
	if err := service.savePreferences("de"); err == nil {
		t.Fatal("unsupported locale accepted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("preference mode=%v", info.Mode())
	}
}

func TestOpenRejectsTraversalAndBackendErrorsDoNotExposeInternalOwnership(t *testing.T) {
	for _, path := range []string{"../outside", "/outside", "a/../../outside", "a//b", "a\\b", "a/./b", "a\x00b"} {
		if _, err := relativeComponents(path); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
	remote := failure(&control.RemoteError{Code: "BatchOwnedProjection", Message: "internal ownership information"})
	if remote.Detail != "" {
		t.Fatal("backend implementation detail leaked")
	}
}

func TestAuthorizedReadCanPreviewUnreadableConfiguration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root reader")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	original, err := config.ParseDocument("# privately readable\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original.Text), 0o000); err != nil {
		t.Fatal(err)
	}
	service := New(Options{ConfigPath: path, Admin: func(_ context.Context, args []string, _ any) (json.RawMessage, error) {
		if args[0] != "read-config" {
			t.Fatalf("unexpected authorization %v", args)
		}
		return json.Marshal(original)
	}})
	response := service.Execute(context.Background(), Request{Command: "authorize_configuration"})
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	preview, err := service.previewConfiguration(Request{Digest: original.Digest, Fields: map[string]string{"fuse.attr_timeout": "3s"}})
	if err != nil || preview.Document.Fields["fuse.attr_timeout"] != "3s" {
		t.Fatalf("authorized preview=%+v %v", preview, err)
	}
}

func TestRestartDoesNotAdoptAnUnreachableSavedSocket(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	document, err := config.PatchDocument("", map[string]string{"control.socket_path": filepath.Join(root, "new.sock")})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(document.Text), 0o600)
	service := New(Options{ConfigPath: path, Admin: func(context.Context, []string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }, SystemState: func(context.Context) SystemState { return SystemState{Installed: true, Available: true} }})
	previous := filepath.Join(root, "old.sock")
	service.activeSocket = previous
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := service.serviceAction(ctx, "restart"); err == nil {
		t.Fatal("unreachable service reported ready")
	}
	if service.activeSocket != previous {
		t.Fatalf("adopted an unverified socket: %s", service.activeSocket)
	}
}
