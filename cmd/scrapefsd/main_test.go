package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	server := &recordingControlRuntime{
		done:    make(chan struct{}),
		events:  &events,
		stopErr: stop,
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	startControl := func(
		_ string,
		_ os.FileMode,
		_ uint32,
		service control.BatchService,
		libraryService control.LibraryService,
		projectionService control.ProjectionService,
		gcService control.GCService,
	) (controlRuntime, error) {
		if service == nil {
			t.Fatal("control service is nil")
		}
		if libraryService == nil {
			t.Fatal("Library control service is nil")
		}
		if projectionService == nil {
			t.Fatal("Projection control service is nil")
		}
		if gcService == nil {
			t.Fatal("GC control service is nil")
		}
		events = append(events, "control")
		return server, nil
	}

	err = serveRuntimeWithDependencies(
		ctx,
		configuration,
		opened,
		runtimeDependencies{
			newIndex: func(*namespace.Repository) runtimeIndex {
				return &recordingRuntimeIndex{events: &events}
			},
			mount:        mount,
			startControl: startControl,
			notifyReady: func() error {
				events = append(events, "ready")
				cancel()
				return nil
			},
			warnings: io.Discard,
		},
	)
	if !errors.Is(err, stop) || !errors.Is(err, unmountErr) {
		t.Fatalf("serveRuntime error = %v, want control and unmount errors", err)
	}
	want := "index,mount,control,ready,stop,wait,unmount,close"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("runtime events = %q, want %q", got, want)
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
	err = serveRuntimeWithDependencies(
		context.Background(),
		config.Config{
			Storage: config.Storage{OverlayStorePath: storePath},
		},
		opened,
		runtimeDependencies{
			newIndex: func(*namespace.Repository) runtimeIndex {
				return &recordingRuntimeIndex{}
			},
			mount: func(
				context.Context,
				config.FUSE,
				*namespace.Repository,
				*objectstore.Store,
			) (mountedFilesystem, error) {
				mountCalled = true
				return recordingMount{}, nil
			},
			startControl: func(
				string,
				os.FileMode,
				uint32,
				control.BatchService,
				control.LibraryService,
				control.ProjectionService,
				control.GCService,
			) (controlRuntime, error) {
				controlCalled = true
				return nil, nil
			},
			notifyReady: func() error {
				return nil
			},
			warnings: io.Discard,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "object store") {
		t.Fatalf("serveRuntime error = %v, want object-store error", err)
	}
	if mountCalled || controlCalled {
		t.Fatal("FUSE or control started after object-store initialization failure")
	}
}

func TestServeRuntimeDoesNotSignalReadyAfterControlStops(t *testing.T) {
	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	controlErr := errors.New("control accept loop failed")
	var events []string
	done := make(chan struct{})
	close(done)
	server := &recordingControlRuntime{
		done:   done,
		events: &events,
		err:    controlErr,
	}
	err = serveRuntimeWithDependencies(
		context.Background(),
		config.Config{
			Storage: config.Storage{
				OverlayStorePath: filepath.Join(root, "objects"),
			},
		},
		opened,
		runtimeDependencies{
			newIndex: func(*namespace.Repository) runtimeIndex {
				return &recordingRuntimeIndex{events: &events}
			},
			mount: func(
				context.Context,
				config.FUSE,
				*namespace.Repository,
				*objectstore.Store,
			) (mountedFilesystem, error) {
				events = append(events, "mount")
				return recordingMount{unmount: func() error {
					events = append(events, "unmount")
					return nil
				}}, nil
			},
			startControl: func(
				string,
				os.FileMode,
				uint32,
				control.BatchService,
				control.LibraryService,
				control.ProjectionService,
				control.GCService,
			) (controlRuntime, error) {
				events = append(events, "control")
				return server, nil
			},
			notifyReady: func() error {
				events = append(events, "ready")
				return nil
			},
			warnings: io.Discard,
		},
	)
	if !errors.Is(err, controlErr) {
		t.Fatalf("serveRuntimeWithDependencies error = %v, want control error", err)
	}
	want := "index,mount,control,stop,wait,unmount,close"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("runtime events = %q, want %q", got, want)
	}
}

func TestServeRuntimeDoesNotSignalReadyAfterStartupCancellation(t *testing.T) {
	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	var events []string
	ctx, cancel := context.WithCancel(context.Background())
	server := &recordingControlRuntime{
		done:   make(chan struct{}),
		events: &events,
	}
	err = serveRuntimeWithDependencies(
		ctx,
		config.Config{
			Storage: config.Storage{
				OverlayStorePath: filepath.Join(root, "objects"),
			},
		},
		opened,
		runtimeDependencies{
			newIndex: func(*namespace.Repository) runtimeIndex {
				return &recordingRuntimeIndex{events: &events}
			},
			mount: func(
				context.Context,
				config.FUSE,
				*namespace.Repository,
				*objectstore.Store,
			) (mountedFilesystem, error) {
				events = append(events, "mount")
				return recordingMount{unmount: func() error {
					events = append(events, "unmount")
					return nil
				}}, nil
			},
			startControl: func(
				string,
				os.FileMode,
				uint32,
				control.BatchService,
				control.LibraryService,
				control.ProjectionService,
				control.GCService,
			) (controlRuntime, error) {
				events = append(events, "control")
				cancel()
				return server, nil
			},
			notifyReady: func() error {
				events = append(events, "ready")
				return nil
			},
			warnings: io.Discard,
		},
	)
	if err != nil {
		t.Fatalf("serveRuntimeWithDependencies: %v", err)
	}
	want := "index,mount,control,stop,wait,unmount,close"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("runtime events = %q, want %q", got, want)
	}
}

func TestServeRuntimeOrdersReadinessAndGracefulShutdown(t *testing.T) {
	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	var events []string
	ctx, cancel := context.WithCancel(context.Background())
	server := &recordingControlRuntime{
		done:   make(chan struct{}),
		events: &events,
	}
	err = serveRuntimeWithDependencies(
		ctx,
		config.Config{
			Storage: config.Storage{
				OverlayStorePath: filepath.Join(root, "objects"),
			},
		},
		opened,
		runtimeDependencies{
			newIndex: func(*namespace.Repository) runtimeIndex {
				return &recordingRuntimeIndex{events: &events}
			},
			mount: func(
				context.Context,
				config.FUSE,
				*namespace.Repository,
				*objectstore.Store,
			) (mountedFilesystem, error) {
				events = append(events, "mount")
				return recordingMount{unmount: func() error {
					events = append(events, "unmount")
					return nil
				}}, nil
			},
			startControl: func(
				string,
				os.FileMode,
				uint32,
				control.BatchService,
				control.LibraryService,
				control.ProjectionService,
				control.GCService,
			) (controlRuntime, error) {
				events = append(events, "control")
				return server, nil
			},
			notifyReady: func() error {
				events = append(events, "ready")
				cancel()
				return nil
			},
			warnings: io.Discard,
		},
	)
	if err != nil {
		t.Fatalf("serveRuntimeWithDependencies: %v", err)
	}
	want := "index,mount,control,ready,stop,wait,unmount,close"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("runtime events = %q, want %q", got, want)
	}
	if err := opened.UseWrite(context.Background(), func() error {
		t.Fatal("write ran after runtime shutdown")
		return nil
	}); !errors.Is(err, database.ErrWritesClosed) {
		t.Fatalf("UseWrite after shutdown error = %v, want ErrWritesClosed", err)
	}
}

func TestServeRuntimeStartupFailureMatrixDoesNotSignalReady(t *testing.T) {
	tests := []struct {
		name       string
		indexErr   error
		mountErr   error
		controlErr error
		notifyErr  error
		want       string
	}{
		{
			name:     "index",
			indexErr: errors.New("index failed"),
			want:     "index",
		},
		{
			name:     "mount",
			mountErr: errors.New("mount failed"),
			want:     "index,mount",
		},
		{
			name:       "control",
			controlErr: errors.New("control failed"),
			want:       "index,mount,control,unmount",
		},
		{
			name:      "readiness",
			notifyErr: errors.New("readiness failed"),
			want:      "index,mount,control,notify,stop,wait,unmount,close",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			opened, err := database.OpenWithOptions(
				context.Background(),
				filepath.Join(root, "state", "scrapefs.db"),
				database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
			)
			if err != nil {
				t.Fatalf("OpenWithOptions: %v", err)
			}
			defer opened.Close()

			var events []string
			server := &recordingControlRuntime{
				done:   make(chan struct{}),
				events: &events,
			}
			err = serveRuntimeWithDependencies(
				context.Background(),
				config.Config{
					Storage: config.Storage{
						OverlayStorePath: filepath.Join(root, "objects"),
					},
				},
				opened,
				runtimeDependencies{
					newIndex: func(*namespace.Repository) runtimeIndex {
						return &recordingRuntimeIndex{
							events: &events,
							err:    test.indexErr,
						}
					},
					mount: func(
						context.Context,
						config.FUSE,
						*namespace.Repository,
						*objectstore.Store,
					) (mountedFilesystem, error) {
						events = append(events, "mount")
						if test.mountErr != nil {
							return nil, test.mountErr
						}
						return recordingMount{unmount: func() error {
							events = append(events, "unmount")
							return nil
						}}, nil
					},
					startControl: func(
						string,
						os.FileMode,
						uint32,
						control.BatchService,
						control.LibraryService,
						control.ProjectionService,
						control.GCService,
					) (controlRuntime, error) {
						events = append(events, "control")
						if test.controlErr != nil {
							return nil, test.controlErr
						}
						return server, nil
					},
					notifyReady: func() error {
						events = append(events, "notify")
						return test.notifyErr
					},
					warnings: io.Discard,
				},
			)
			if err == nil || !errors.Is(err, firstError(
				test.indexErr,
				test.mountErr,
				test.controlErr,
				test.notifyErr,
			)) {
				t.Fatalf("serveRuntimeWithDependencies error = %v", err)
			}
			if got := strings.Join(events, ","); got != test.want {
				t.Fatalf("runtime events = %q, want %q", got, test.want)
			}
		})
	}
}

func TestServeRuntimeWarnsWithoutFailingCommittedMutations(t *testing.T) {
	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	rebuildErr := errors.New("refresh failed")
	invalidationErr := errors.New("invalidation failed")
	index := &scriptedRuntimeIndex{
		results: []runtimeIndexResult{
			{},
			{changes: namespace.ChangeSet{InodeIDs: []int64{1}}},
			{err: rebuildErr},
		},
	}
	mounted := &recordingInvalidatingMount{err: invalidationErr}
	var repository *namespace.Repository
	var mutationErr error
	var warnings bytes.Buffer
	var events []string
	ctx, cancel := context.WithCancel(context.Background())
	server := &recordingControlRuntime{
		done:   make(chan struct{}),
		events: &events,
	}
	err = serveRuntimeWithDependencies(
		ctx,
		config.Config{
			Storage: config.Storage{
				OverlayStorePath: filepath.Join(root, "objects"),
			},
		},
		opened,
		runtimeDependencies{
			newIndex: func(*namespace.Repository) runtimeIndex {
				return index
			},
			mount: func(
				_ context.Context,
				_ config.FUSE,
				got *namespace.Repository,
				_ *objectstore.Store,
			) (mountedFilesystem, error) {
				repository = got
				return mounted, nil
			},
			startControl: func(
				string,
				os.FileMode,
				uint32,
				control.BatchService,
				control.LibraryService,
				control.ProjectionService,
				control.GCService,
			) (controlRuntime, error) {
				return server, nil
			},
			notifyReady: func() error {
				var library namespace.Library
				_, createErr := repository.Update(
					context.Background(),
					runtimeOperation(
						"library.create",
						[]string{"media"},
						[]namespace.NodeKind{namespace.LibraryRoot},
					),
					func(transaction *namespace.Transaction) error {
						var err error
						library, err = transaction.CreateLibrary(
							context.Background(),
							runtimeLibraryRequest("media"),
						)
						return err
					},
				)
				_, renameErr := repository.Update(
					context.Background(),
					runtimeOperation(
						"library.rename",
						[]string{"media", "movies"},
						[]namespace.NodeKind{
							namespace.LibraryRoot,
							namespace.LibraryRoot,
						},
					),
					func(transaction *namespace.Transaction) error {
						_, err := transaction.RenameLibrary(
							context.Background(),
							library.Name,
							"movies",
						)
						return err
					},
				)
				mutationErr = errors.Join(createErr, renameErr)
				cancel()
				return nil
			},
			warnings: &warnings,
		},
	)
	if err != nil {
		t.Fatalf("serveRuntimeWithDependencies: %v", err)
	}
	if mutationErr != nil {
		t.Fatalf("committed mutations returned warning error: %v", mutationErr)
	}
	if len(mounted.commits) != 1 ||
		mounted.commits[0].Operation != "library.create" {
		t.Fatalf("invalidation commits = %#v, want library.create", mounted.commits)
	}
	if !strings.Contains(warnings.String(), invalidationErr.Error()) ||
		!strings.Contains(warnings.String(), "rebuild namespace index after library.rename") ||
		!strings.Contains(warnings.String(), rebuildErr.Error()) {
		t.Fatalf("warnings = %q", warnings.String())
	}
}

func TestServeRuntimeDrainsAdmittedWriteBeforeUnmount(t *testing.T) {
	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	var repository *namespace.Repository
	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeDone := make(chan error, 1)
	unmounted := make(chan struct{})
	shutdownStarted := make(chan struct{})
	var events []string
	ctx, cancel := context.WithCancel(context.Background())
	server := &recordingControlRuntime{
		done:   make(chan struct{}),
		events: &events,
		stop: func() {
			close(shutdownStarted)
		},
	}
	runtimeDone := make(chan error, 1)
	go func() {
		runtimeDone <- serveRuntimeWithDependencies(
			ctx,
			config.Config{
				Storage: config.Storage{
					OverlayStorePath: filepath.Join(root, "objects"),
				},
			},
			opened,
			runtimeDependencies{
				newIndex: func(*namespace.Repository) runtimeIndex {
					return &recordingRuntimeIndex{}
				},
				mount: func(
					_ context.Context,
					_ config.FUSE,
					got *namespace.Repository,
					_ *objectstore.Store,
				) (mountedFilesystem, error) {
					repository = got
					return recordingMount{unmount: func() error {
						close(unmounted)
						return nil
					}}, nil
				},
				startControl: func(
					string,
					os.FileMode,
					uint32,
					control.BatchService,
					control.LibraryService,
					control.ProjectionService,
					control.GCService,
				) (controlRuntime, error) {
					return server, nil
				},
				notifyReady: func() error {
					go func() {
						_, err := repository.Update(
							context.Background(),
							runtimeOperation(
								"library.create",
								[]string{"media"},
								[]namespace.NodeKind{namespace.LibraryRoot},
							),
							func(transaction *namespace.Transaction) error {
								close(writeEntered)
								<-releaseWrite
								_, err := transaction.CreateLibrary(
									context.Background(),
									runtimeLibraryRequest("media"),
								)
								return err
							},
						)
						writeDone <- err
					}()
					<-writeEntered
					cancel()
					return nil
				},
				warnings: io.Discard,
			},
		)
	}()

	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("runtime did not begin shutdown")
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := opened.TryUseWrite(context.Background(), func() error {
			t.Fatal("new write ran during shutdown")
			return nil
		})
		if errors.Is(err, database.ErrWritesClosed) {
			break
		}
		if !errors.Is(err, database.ErrWriteInProgress) {
			t.Fatalf("TryUseWrite during shutdown error = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not close write admission")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-unmounted:
		t.Fatal("FUSE unmounted before admitted write completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseWrite)
	if err := <-writeDone; err != nil {
		t.Fatalf("admitted Update: %v", err)
	}
	select {
	case err := <-runtimeDone:
		if err != nil {
			t.Fatalf("serveRuntimeWithDependencies: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not finish after admitted write completed")
	}
	if _, err := repository.Library(context.Background(), "media"); err != nil {
		t.Fatalf("drained Library commit: %v", err)
	}
}

func TestServeRuntimeRebuildsPersistedStateAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	lockPath := filepath.Join(root, "run", "scrapefsd.lock")
	storePath := filepath.Join(root, "objects")
	opened, err := database.OpenWithOptions(
		context.Background(),
		databasePath,
		database.Options{LockPath: lockPath},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	repository := namespace.NewRepository(opened)
	var library namespace.Library
	if _, err := repository.Update(
		context.Background(),
		runtimeOperation(
			"library.create",
			[]string{"media"},
			[]namespace.NodeKind{namespace.LibraryRoot},
		),
		func(transaction *namespace.Transaction) error {
			var err error
			library, err = transaction.CreateLibrary(
				context.Background(),
				runtimeLibraryRequest("media"),
			)
			return err
		},
	); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	store, err := objectstore.OpenWithOptions(storePath, opened, objectstore.Options{
		Repository: repository,
	})
	if err != nil {
		t.Fatalf("OpenWithOptions object store: %v", err)
	}
	handle, node, err := store.Create(
		context.Background(),
		runtimeOperation(
			"overlay.create",
			[]string{"media/poster.jpg"},
			[]namespace.NodeKind{namespace.OverlayFile},
		),
		namespace.CreateOverlay{
			ParentID: library.RootNodeID,
			Name:     "poster.jpg",
			Creator:  namespace.Actor{Kind: namespace.ActorSystem},
			Attributes: namespace.Attributes{
				Mode: 0o640,
				UID:  1000,
				GID:  1000,
			},
		},
		strings.NewReader("poster"),
	)
	if err != nil {
		t.Fatalf("Create overlay: %v", err)
	}
	objectID := *node.ObjectID
	if err := handle.Close(); err != nil {
		t.Fatalf("overlay handle Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("object store Close: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("database Close: %v", err)
	}

	for restart := 1; restart <= 2; restart++ {
		opened, err := database.OpenWithOptions(
			context.Background(),
			databasePath,
			database.Options{LockPath: lockPath},
		)
		if err != nil {
			t.Fatalf("restart %d OpenWithOptions: %v", restart, err)
		}
		var stats namespace.IndexStats
		var content string
		var events []string
		ctx, cancel := context.WithCancel(context.Background())
		server := &recordingControlRuntime{
			done:   make(chan struct{}),
			events: &events,
		}
		err = serveRuntimeWithDependencies(
			ctx,
			config.Config{
				Storage: config.Storage{OverlayStorePath: storePath},
			},
			opened,
			runtimeDependencies{
				newIndex: func(repository *namespace.Repository) runtimeIndex {
					return &capturingRuntimeIndex{
						index: namespace.NewIndex(repository),
						stats: &stats,
					}
				},
				mount: func(
					ctx context.Context,
					_ config.FUSE,
					_ *namespace.Repository,
					store *objectstore.Store,
				) (mountedFilesystem, error) {
					file, err := store.OpenObject(ctx, objectID, os.O_RDONLY)
					if err != nil {
						return nil, err
					}
					data, readErr := io.ReadAll(file)
					closeErr := file.Close()
					if readErr != nil || closeErr != nil {
						return nil, errors.Join(readErr, closeErr)
					}
					content = string(data)
					return recordingMount{}, nil
				},
				startControl: func(
					string,
					os.FileMode,
					uint32,
					control.BatchService,
					control.LibraryService,
					control.ProjectionService,
					control.GCService,
				) (controlRuntime, error) {
					return server, nil
				},
				notifyReady: func() error {
					cancel()
					return nil
				},
				warnings: io.Discard,
			},
		)
		if err != nil {
			t.Fatalf("restart %d serveRuntimeWithDependencies: %v", restart, err)
		}
		if stats != (namespace.IndexStats{Libraries: 1, Nodes: 3, Objects: 1}) {
			t.Errorf("restart %d index stats = %#v", restart, stats)
		}
		if content != "poster" {
			t.Errorf("restart %d object content = %q, want poster", restart, content)
		}
		if err := opened.Close(); err != nil {
			t.Fatalf("restart %d database Close: %v", restart, err)
		}
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

type recordingInvalidatingMount struct {
	commits []namespace.Commit
	changes []namespace.ChangeSet
	err     error
}

func (*recordingInvalidatingMount) Unmount() error {
	return nil
}

func (mounted *recordingInvalidatingMount) Invalidate(
	commit namespace.Commit,
	changes namespace.ChangeSet,
) []error {
	mounted.commits = append(mounted.commits, commit)
	mounted.changes = append(mounted.changes, changes)
	if mounted.err == nil {
		return nil
	}
	return []error{mounted.err}
}

type recordingRuntimeIndex struct {
	events *[]string
	err    error
}

func (index *recordingRuntimeIndex) Rebuild(
	context.Context,
) (namespace.ChangeSet, error) {
	if index.events != nil {
		*index.events = append(*index.events, "index")
	}
	return namespace.ChangeSet{}, index.err
}

type runtimeIndexResult struct {
	changes namespace.ChangeSet
	err     error
}

type scriptedRuntimeIndex struct {
	results []runtimeIndexResult
	calls   int
}

func (index *scriptedRuntimeIndex) Rebuild(
	context.Context,
) (namespace.ChangeSet, error) {
	if index.calls >= len(index.results) {
		return namespace.ChangeSet{}, errors.New("unexpected index rebuild")
	}
	result := index.results[index.calls]
	index.calls++
	return result.changes, result.err
}

type capturingRuntimeIndex struct {
	index *namespace.Index
	stats *namespace.IndexStats
}

func (index *capturingRuntimeIndex) Rebuild(
	ctx context.Context,
) (namespace.ChangeSet, error) {
	changes, err := index.index.Rebuild(ctx)
	if err == nil {
		*index.stats = index.index.Stats()
	}
	return changes, err
}

type recordingControlRuntime struct {
	done    chan struct{}
	events  *[]string
	err     error
	stopErr error
	stop    func()
}

func (server *recordingControlRuntime) Done() <-chan struct{} {
	return server.done
}

func (server *recordingControlRuntime) Err() error {
	return server.err
}

func (server *recordingControlRuntime) StopAccepting() error {
	if server.events != nil {
		*server.events = append(*server.events, "stop")
	}
	if server.stop != nil {
		server.stop()
	}
	return server.stopErr
}

func (server *recordingControlRuntime) Wait() error {
	if server.events != nil {
		*server.events = append(*server.events, "wait")
	}
	return nil
}

func (server *recordingControlRuntime) Close() error {
	if server.events != nil {
		*server.events = append(*server.events, "close")
	}
	return nil
}

func runtimeOperation(
	name string,
	paths []string,
	kinds []namespace.NodeKind,
) namespace.Operation {
	return namespace.Operation{
		Name:  name,
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
		Details: namespace.OperationDetails{
			Paths:     paths,
			NodeKinds: kinds,
		},
	}
}

func runtimeLibraryRequest(name string) namespace.CreateLibrary {
	return namespace.CreateLibrary{
		Name:           name,
		ProfileName:    "jellyfin",
		ProfileVersion: 1,
		Creator:        namespace.Actor{Kind: namespace.ActorSystem},
		UID:            1000,
		GID:            1000,
		RootMode:       0o775,
		DirectoryMode:  0o775,
		ProjectionMode: 0o444,
		OverlayMode:    0o664,
	}
}

func firstError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
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
