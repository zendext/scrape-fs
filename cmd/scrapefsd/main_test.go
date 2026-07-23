package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/database"
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

func TestRunInitializesDatabaseWithoutCreatingOtherRuntimeState(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.toml")
	databasePath := filepath.Join(tempDir, "state", "scrapefs.db")
	overlayPath := filepath.Join(tempDir, "objects")
	mountpoint := filepath.Join(tempDir, "mount")
	socketPath := filepath.Join(tempDir, "control.sock")
	configText := `
[storage]
database_path = "` + databasePath + `"
overlay_store_path = "` + overlayPath + `"

[fuse]
mountpoint = "` + mountpoint + `"

[control]
socket_path = "` + socketPath + `"
`
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	openDatabase := func(path string) (databaseCloser, error) {
		return database.OpenWithOptions(
			context.Background(),
			path,
			database.Options{LockPath: filepath.Join(tempDir, "run", "scrapefsd.lock")},
		)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := runWithDependencies(
		[]string{"--config", configPath},
		&stdout,
		&stderr,
		config.Load,
		openDatabase,
	); exitCode != 0 {
		t.Fatalf("runWithDependencies exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}

	if info, err := os.Stat(databasePath); err != nil {
		t.Fatalf("database Stat: %v", err)
	} else if !info.Mode().IsRegular() {
		t.Fatalf("database mode = %v, want regular file", info.Mode())
	}
	for name, path := range map[string]string{
		"overlay store": overlayPath,
		"mountpoint":    mountpoint,
		"socket":        socketPath,
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stat error = %v, want not exist", name, err)
		}
	}
}

func TestRunReportsDatabaseInitializationErrorBeforeOtherRuntimeState(t *testing.T) {
	tempDir := t.TempDir()
	overlayPath := filepath.Join(tempDir, "objects")
	mountpoint := filepath.Join(tempDir, "mount")
	socketPath := filepath.Join(tempDir, "control.sock")
	loader := func(string) (config.Config, error) {
		return config.Config{
			Storage: config.Storage{
				DatabasePath:     filepath.Join(tempDir, "state", "scrapefs.db"),
				OverlayStorePath: overlayPath,
			},
			FUSE: config.FUSE{Mountpoint: mountpoint},
			Control: config.Control{
				SocketPath: socketPath,
			},
		}, nil
	}
	openDatabase := func(string) (databaseCloser, error) {
		return nil, errors.New("database initialization failed")
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDependencies(nil, &stdout, &stderr, loader, openDatabase)

	if exitCode != 1 {
		t.Fatalf("runWithDependencies exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "database initialization failed") {
		t.Errorf("stderr = %q", stderr.String())
	}
	for name, path := range map[string]string{
		"overlay store": overlayPath,
		"mountpoint":    mountpoint,
		"socket":        socketPath,
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stat error = %v, want not exist", name, err)
		}
	}
}

func TestRunServesControlUntilContextCancellationAndClosesDatabase(t *testing.T) {
	tempDir := t.TempDir()
	configuration := config.Config{
		Storage: config.Storage{
			DatabasePath:     filepath.Join(tempDir, "state", "scrapefs.db"),
			OverlayStorePath: filepath.Join(tempDir, "objects"),
		},
		FUSE: config.FUSE{Mountpoint: filepath.Join(tempDir, "mount")},
		Control: config.Control{
			SocketPath: filepath.Join(tempDir, "control.sock"),
			SocketMode: 0o660,
		},
	}
	loader := func(string) (config.Config, error) {
		return configuration, nil
	}
	opened := &recordingDatabase{}
	openDatabase := func(path string) (databaseCloser, error) {
		if path != configuration.Storage.DatabasePath {
			t.Errorf("database path = %q, want %q", path, configuration.Storage.DatabasePath)
		}
		return opened, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveStarted := make(chan struct{})
	serve := func(
		serveContext context.Context,
		got config.Config,
		database databaseCloser,
	) error {
		if got != configuration {
			t.Errorf("serve config = %#v, want %#v", got, configuration)
		}
		if database != opened {
			t.Errorf("serve database = %T, want recordingDatabase", database)
		}
		close(serveStarted)
		<-serveContext.Done()
		return nil
	}
	go func() {
		<-serveStarted
		cancel()
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithServerDependencies(
		ctx,
		nil,
		&stdout,
		&stderr,
		loader,
		openDatabase,
		serve,
	)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !opened.closed {
		t.Fatal("database was not closed after control server stopped")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("stdout=%q stderr=%q, want empty", stdout.String(), stderr.String())
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

type recordingDatabase struct {
	closed bool
}

func (database *recordingDatabase) Close() error {
	database.closed = true
	return nil
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
