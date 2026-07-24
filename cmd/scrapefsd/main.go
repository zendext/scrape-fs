package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/zendext/scrape-fs/internal/buildinfo"
	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/control"
	"github.com/zendext/scrape-fs/internal/database"
	fusefs "github.com/zendext/scrape-fs/internal/fuse"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

type cli struct {
	Config  string           `help:"Path to the TOML configuration file." default:"${default_config}"`
	Version kong.VersionFlag `help:"Print version and exit."`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()
	return runWithServerDependencies(
		ctx,
		args,
		stdout,
		stderr,
		config.Load,
		func(path string) (databaseCloser, error) {
			return database.Open(context.Background(), path)
		},
		func(
			ctx context.Context,
			configuration config.Config,
			opened databaseCloser,
		) error {
			return serveRuntime(ctx, configuration, opened, stderr)
		},
	)
}

type configLoader func(string) (config.Config, error)
type databaseCloser interface {
	Close() error
}
type databaseOpener func(string) (databaseCloser, error)
type daemonServer func(context.Context, config.Config, databaseCloser) error
type mountedFilesystem interface {
	Unmount() error
}
type filesystemMounter func(
	context.Context,
	config.FUSE,
	*namespace.Repository,
	*objectstore.Store,
) (mountedFilesystem, error)
type runtimeIndex interface {
	Rebuild(context.Context) (namespace.ChangeSet, error)
}
type controlRuntime interface {
	Done() <-chan struct{}
	Err() error
	StopAccepting() error
	Wait() error
	Close() error
}
type controlStarter func(
	string,
	os.FileMode,
	uint32,
	control.BatchService,
	control.LibraryService,
	control.ProjectionService,
	control.GCService,
) (controlRuntime, error)
type runtimeDependencies struct {
	newIndex     func(*namespace.Repository) runtimeIndex
	mount        filesystemMounter
	startControl controlStarter
	notifyReady  func() error
	warnings     io.Writer
}
type filesystemInvalidator interface {
	Invalidate(namespace.Commit, namespace.ChangeSet) []error
}

func serveRuntime(
	ctx context.Context,
	configuration config.Config,
	opened databaseCloser,
	warnings io.Writer,
) error {
	return serveRuntimeWithDependencies(
		ctx,
		configuration,
		opened,
		runtimeDependencies{
			newIndex: func(repository *namespace.Repository) runtimeIndex {
				return namespace.NewIndex(repository)
			},
			mount: mountFilesystem,
			startControl: func(
				socketPath string,
				mode os.FileMode,
				administratorUID uint32,
				service control.BatchService,
				libraryService control.LibraryService,
				projectionService control.ProjectionService,
				gcService control.GCService,
			) (controlRuntime, error) {
				return control.Start(
					socketPath,
					mode,
					administratorUID,
					service,
					libraryService,
					projectionService,
					gcService,
				)
			},
			notifyReady: notifyReady,
			warnings:    warnings,
		},
	)
}

func serveRuntimeWithDependencies(
	ctx context.Context,
	configuration config.Config,
	opened databaseCloser,
	dependencies runtimeDependencies,
) error {
	openedDatabase, ok := opened.(*database.DB)
	if !ok {
		return fmt.Errorf("unsupported database implementation %T", opened)
	}
	if dependencies.newIndex == nil {
		return errors.New("runtime index builder is required")
	}
	if dependencies.mount == nil {
		return errors.New("runtime filesystem mounter is required")
	}
	if dependencies.startControl == nil {
		return errors.New("runtime control starter is required")
	}
	if dependencies.notifyReady == nil {
		return errors.New("runtime readiness notifier is required")
	}
	if dependencies.warnings == nil {
		dependencies.warnings = io.Discard
	}

	repository := namespace.NewRepository(openedDatabase)
	store, err := objectstore.OpenWithOptions(
		configuration.Storage.OverlayStorePath,
		openedDatabase,
		objectstore.Options{
			OrphanGracePeriod: configuration.GC.OrphanGracePeriod,
			Repository:        repository,
		},
	)
	if err != nil {
		return fmt.Errorf("initialize object store: %w", err)
	}

	index := dependencies.newIndex(repository)
	if index == nil {
		return errors.Join(
			errors.New("runtime index builder returned nil"),
			store.Close(),
		)
	}
	if _, err := index.Rebuild(ctx); err != nil {
		closeErr := store.Close()
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return closeErr
		}
		return errors.Join(
			fmt.Errorf("rebuild namespace index: %w", err),
			closeErr,
		)
	}
	if ctx.Err() != nil {
		return store.Close()
	}

	var invalidatorMu sync.RWMutex
	var invalidator filesystemInvalidator
	repository.AddCommitListener(func(commit namespace.Commit) {
		changes, err := index.Rebuild(context.Background())
		if err != nil {
			fmt.Fprintf(
				dependencies.warnings,
				"scrapefsd: warning: rebuild namespace index after %s: %v\n",
				commit.Operation,
				err,
			)
			return
		}
		invalidatorMu.RLock()
		current := invalidator
		invalidatorMu.RUnlock()
		if current == nil {
			return
		}
		for _, err := range current.Invalidate(commit, changes) {
			fmt.Fprintf(
				dependencies.warnings,
				"scrapefsd: warning: %v\n",
				err,
			)
		}
	})

	mounted, err := dependencies.mount(
		ctx,
		configuration.FUSE,
		repository,
		store,
	)
	if err != nil {
		closeErr := store.Close()
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return closeErr
		}
		return errors.Join(err, closeErr)
	}
	if mounted == nil {
		return errors.Join(
			errors.New("runtime filesystem mounter returned nil"),
			store.Close(),
		)
	}
	if current, ok := mounted.(filesystemInvalidator); ok {
		invalidatorMu.Lock()
		invalidator = current
		invalidatorMu.Unlock()
	}
	if ctx.Err() != nil {
		return shutdownRuntime(openedDatabase, mounted, nil, store)
	}

	server, err := dependencies.startControl(
		configuration.Control.SocketPath,
		configuration.Control.SocketMode,
		configuration.Control.AdministratorUID,
		projectionbatch.NewService(repository),
		library.NewServiceWithObjectReclaimer(repository, store),
		projection.NewService(repository),
		store,
	)
	if err != nil {
		return errors.Join(
			fmt.Errorf("start control server: %w", err),
			shutdownRuntime(openedDatabase, mounted, server, store),
		)
	}
	if server == nil {
		return errors.Join(
			errors.New("runtime control starter returned nil"),
			shutdownRuntime(openedDatabase, mounted, nil, store),
		)
	}

	var runtimeErr error
	if ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-server.Done():
			runtimeErr = stoppedControlError(server)
		default:
			if err := dependencies.notifyReady(); err != nil {
				runtimeErr = fmt.Errorf("signal readiness: %w", err)
				break
			}
			select {
			case <-ctx.Done():
			case <-server.Done():
				runtimeErr = stoppedControlError(server)
			}
		}
	}
	return errors.Join(
		runtimeErr,
		shutdownRuntime(openedDatabase, mounted, server, store),
	)
}

func shutdownRuntime(
	openedDatabase *database.DB,
	mounted mountedFilesystem,
	server controlRuntime,
	store *objectstore.Store,
) error {
	var shutdownErr error
	if server != nil {
		shutdownErr = errors.Join(shutdownErr, server.StopAccepting())
	}
	openedDatabase.StopWrites()
	if server != nil {
		shutdownErr = errors.Join(shutdownErr, server.Wait())
	}
	if mounted != nil {
		shutdownErr = errors.Join(shutdownErr, mounted.Unmount())
	}
	if server != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	if store != nil {
		shutdownErr = errors.Join(shutdownErr, store.Close())
	}
	return shutdownErr
}

func stoppedControlError(server controlRuntime) error {
	if err := server.Err(); err != nil {
		return err
	}
	return errors.New("control server stopped unexpectedly")
}

func mountFilesystem(
	ctx context.Context,
	configuration config.FUSE,
	repository *namespace.Repository,
	store *objectstore.Store,
) (mountedFilesystem, error) {
	return fusefs.Mount(ctx, configuration, repository, store)
}

func runWithLoader(args []string, stdout, stderr io.Writer, load configLoader) int {
	return runWithDependencies(
		args,
		stdout,
		stderr,
		load,
		func(string) (databaseCloser, error) {
			return noopDatabase{}, nil
		},
	)
}

func runWithDependencies(
	args []string,
	stdout, stderr io.Writer,
	load configLoader,
	openDatabase databaseOpener,
) int {
	return runWithServerDependencies(
		context.Background(),
		args,
		stdout,
		stderr,
		load,
		openDatabase,
		func(context.Context, config.Config, databaseCloser) error {
			return nil
		},
	)
}

func runWithServerDependencies(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	load configLoader,
	openDatabase databaseOpener,
	serve daemonServer,
) int {
	commandLine := cli{}
	requestedExitCode := -1
	parser, err := kong.New(
		&commandLine,
		kong.Name("scrapefsd"),
		kong.Description("ScrapeFS daemon."),
		kong.Vars{
			"default_config": config.DefaultPath,
			"version":        "scrapefsd " + buildinfo.Version,
		},
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) {
			requestedExitCode = code
		}),
	)
	if err != nil {
		fmt.Fprintf(stderr, "scrapefsd: %v\n", err)
		return 1
	}

	if _, err := parser.Parse(args); requestedExitCode >= 0 {
		return requestedExitCode
	} else if err != nil {
		fmt.Fprintf(stderr, "scrapefsd: %v\n", err)
		return 2
	}

	configuration, err := load(commandLine.Config)
	if err != nil {
		fmt.Fprintf(stderr, "scrapefsd: %v\n", err)
		return 1
	}

	database, err := openDatabase(configuration.Storage.DatabasePath)
	if err != nil {
		fmt.Fprintf(stderr, "scrapefsd: initialize database: %v\n", err)
		return 1
	}
	serveErr := serve(ctx, configuration, database)
	closeErr := database.Close()
	if serveErr != nil {
		fmt.Fprintf(
			stderr,
			"scrapefsd: serve daemon: %v\n",
			errors.Join(serveErr, closeErr),
		)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "scrapefsd: close database: %v\n", closeErr)
		return 1
	}
	return 0
}

type noopDatabase struct{}

func (noopDatabase) Close() error {
	return nil
}
