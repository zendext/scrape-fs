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
	"github.com/zendext/scrape-fs/internal/control"
	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
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

func TestServeRuntimeInitializesObjectStoreBeforeControl(t *testing.T) {
	root := t.TempDir()
	configuration := config.Config{
		Storage: config.Storage{
			OverlayStorePath: filepath.Join(root, "objects"),
		},
		Control: config.Control{
			SocketPath: filepath.Join(root, "control.sock"),
			SocketMode: 0o660,
		},
	}
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	stop := errors.New("stop control server")
	unmountErr := errors.New("unmount failed")
	var events []string
	mount := func(
		_ context.Context,
		_ config.FUSE,
		repository *namespace.Repository,
		store *objectstore.Store,
	) (mountedFilesystem, error) {
		if repository == nil {
			t.Fatal("namespace repository is nil")
		}
		if store == nil {
			t.Fatal("object store is nil")
		}
		for _, path := range []string{
			configuration.Storage.OverlayStorePath,
			filepath.Join(configuration.Storage.OverlayStorePath, "staging"),
			filepath.Join(configuration.Storage.OverlayStorePath, "live"),
		} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat(%q): %v", path, err)
			}
			if !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Errorf("%s mode = %v, want directory 0700", path, info.Mode())
			}
		}
		events = append(events, "mount")
		return recordingMount{unmount: func() error {
			events = append(events, "unmount")
			return unmountErr
		}}, nil
	}
	serveControl := func(
		_ context.Context,
		_ string,
		_ os.FileMode,
		_ uint32,
		service control.BatchService,
		libraryService control.LibraryService,
	) error {
		if service == nil {
			t.Fatal("control service is nil")
		}
		if libraryService == nil {
			t.Fatal("Library control service is nil")
		}
		events = append(events, "control")
		return stop
	}

	err = serveRuntime(
		context.Background(),
		configuration,
		opened,
		mount,
		serveControl,
	)
	if !errors.Is(err, stop) || !errors.Is(err, unmountErr) {
		t.Fatalf("serveRuntime error = %v, want control and unmount errors", err)
	}
	if got := strings.Join(events, ","); got != "mount,control,unmount" {
		t.Fatalf("runtime events = %q, want mount,control,unmount", got)
	}
}

func TestServeRuntimeRejectsUnsafeObjectStoreBeforeControl(t *testing.T) {
	root := t.TempDir()
	storePath := filepath.Join(root, "objects")
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatalf("Mkdir object store: %v", err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(storePath, "staging")); err != nil {
		t.Fatalf("Symlink staging: %v", err)
	}
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	mountCalled := false
	controlCalled := false
	err = serveRuntime(
		context.Background(),
		config.Config{
			Storage: config.Storage{OverlayStorePath: storePath},
		},
		opened,
		func(
			context.Context,
			config.FUSE,
			*namespace.Repository,
			*objectstore.Store,
		) (mountedFilesystem, error) {
			mountCalled = true
			return recordingMount{}, nil
		},
		func(
			context.Context,
			string,
			os.FileMode,
			uint32,
			control.BatchService,
			control.LibraryService,
		) error {
			controlCalled = true
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "object store") {
		t.Fatalf("serveRuntime error = %v, want object-store error", err)
	}
	if mountCalled || controlCalled {
		t.Fatal("FUSE or control started after object-store initialization failure")
	}
}

type recordingMount struct {
	unmount func() error
}

func (mounted recordingMount) Unmount() error {
	if mounted.unmount == nil {
		return nil
	}
	return mounted.unmount()
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

func TestRunLegacyAgentConfigCreatesNoRuntimeState(t *testing.T) {
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

[agent]
max_text_file_size = "4MiB"
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
	databaseOpened := false
	openDatabase := func(string) (databaseCloser, error) {
		databaseOpened = true
		return noopDatabase{}, nil
	}
	if exitCode := runWithDependencies(
		[]string{"--config", configPath},
		&stdout,
		&stderr,
		config.Load,
		openDatabase,
	); exitCode != 1 {
		t.Fatalf("runWithDependencies exit code = %d, want 1; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "legacy [agent] table") ||
		!strings.Contains(stderr.String(), "remove") {
		t.Errorf("stderr = %q, want actionable legacy [agent] migration error", stderr.String())
	}
	if databaseOpened {
		t.Fatal("legacy [agent] configuration opened the database")
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
