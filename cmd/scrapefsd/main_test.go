package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zendext/scrape-fs/internal/config"
)

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("run(--help) exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout.String(), "Usage: scrapefsd") {
		t.Errorf("help output = %q, want scrapefsd usage", stdout.String())
	}
	if !strings.Contains(stdout.String(), "-h, --help") {
		t.Errorf("help output = %q, want Kong help flag", stdout.String())
	}
	if !strings.Contains(stdout.String(), "--version") {
		t.Errorf("help output = %q, want version flag", stdout.String())
	}
	if !strings.Contains(stdout.String(), "--config") {
		t.Errorf("help output = %q, want config flag", stdout.String())
	}
	if !strings.Contains(stdout.String(), config.DefaultPath) {
		t.Errorf("help output = %q, want default config path", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("help stderr = %q, want empty", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--version"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("run(--version) exit code = %d, want 0", exitCode)
	}
	if !strings.HasPrefix(stdout.String(), "scrapefsd ") {
		t.Errorf("version output = %q, want scrapefsd prefix", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("version stderr = %q, want empty", stderr.String())
	}
}

func TestRunUsesDefaultConfigPath(t *testing.T) {
	var loadedPath string
	loader := func(path string) (config.Config, error) {
		loadedPath = path
		return config.Config{}, nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithLoader(nil, &stdout, &stderr, loader)

	if exitCode != 0 {
		t.Fatalf("runWithLoader exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if loadedPath != config.DefaultPath {
		t.Errorf("loaded path = %q, want %q", loadedPath, config.DefaultPath)
	}
}

func TestRunUsesConfigOverride(t *testing.T) {
	override := "/tmp/scrapefs-test/config.toml"
	var loadedPath string
	loader := func(path string) (config.Config, error) {
		loadedPath = path
		return config.Config{}, nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithLoader([]string{"--config", override}, &stdout, &stderr, loader)

	if exitCode != 0 {
		t.Fatalf("runWithLoader exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if loadedPath != override {
		t.Errorf("loaded path = %q, want %q", loadedPath, override)
	}
}

func TestHelpAndVersionDoNotLoadConfig(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}} {
		args := args
		t.Run(strings.TrimPrefix(args[0], "--"), func(t *testing.T) {
			called := false
			loader := func(string) (config.Config, error) {
				called = true
				return config.Config{}, nil
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer

			if exitCode := runWithLoader(args, &stdout, &stderr, loader); exitCode != 0 {
				t.Fatalf("runWithLoader(%v) exit code = %d", args, exitCode)
			}
			if called {
				t.Errorf("runWithLoader(%v) loaded configuration", args)
			}
		})
	}
}

func TestInvalidArgumentsDoNotLoadConfig(t *testing.T) {
	for _, args := range [][]string{{"--unknown"}, {"unexpected"}, {"--config"}} {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			called := false
			loader := func(string) (config.Config, error) {
				called = true
				return config.Config{}, nil
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer

			if exitCode := runWithLoader(args, &stdout, &stderr, loader); exitCode != 2 {
				t.Fatalf("runWithLoader(%v) exit code = %d, want 2", args, exitCode)
			}
			if called {
				t.Errorf("runWithLoader(%v) loaded configuration", args)
			}
			if stderr.Len() == 0 {
				t.Errorf("runWithLoader(%v) stderr is empty", args)
			}
		})
	}
}

func TestRunReportsConfigurationError(t *testing.T) {
	loader := func(string) (config.Config, error) {
		return config.Config{}, errors.New("configuration failed")
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithLoader(nil, &stdout, &stderr, loader)

	if exitCode != 1 {
		t.Fatalf("runWithLoader exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "configuration failed") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunRejectsRelativeConfigPath(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--config", "config.toml"}, &stdout, &stderr)

	if exitCode != 1 {
		t.Fatalf("run exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "config path") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunValidatesConfigWithoutCreatingRuntimeState(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.toml")
	configText := `
[storage]
database_path = "` + filepath.Join(tempDir, "state.db") + `"
overlay_store_path = "` + filepath.Join(tempDir, "objects") + `"

[fuse]
mountpoint = "` + filepath.Join(tempDir, "mount") + `"

[control]
socket_path = "` + filepath.Join(tempDir, "control.sock") + `"
`
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir before: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := run([]string{"--config", configPath}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}

	after, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("directory entries changed: before=%v after=%v", entryNames(before), entryNames(after))
	}
}

func TestRunInvalidConfigCreatesNoRuntimeState(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.toml")
	configText := `
[storage]
database_path = "` + filepath.Join(tempDir, "state.db") + `"
overlay_store_path = "` + filepath.Join(tempDir, "objects") + `"

[fuse]
mountpoint = "` + filepath.Join(tempDir, "mount") + `"
entry_timeout = "-1s"

[control]
socket_path = "` + filepath.Join(tempDir, "control.sock") + `"
`
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir before: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := run([]string{"--config", configPath}, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("run exit code = %d, want 1; stderr=%q", exitCode, stderr.String())
	}

	after, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("directory entries changed: before=%v after=%v", entryNames(before), entryNames(after))
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
