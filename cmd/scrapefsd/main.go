package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
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
			return serveRuntime(
				ctx,
				configuration,
				opened,
				mountFilesystem,
				control.Serve,
			)
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
type controlServer func(
	context.Context,
	string,
	os.FileMode,
	uint32,
	control.BatchService,
	control.LibraryService,
	control.ProjectionService,
) error

func serveRuntime(
	ctx context.Context,
	configuration config.Config,
	opened databaseCloser,
	mount filesystemMounter,
	serveControl controlServer,
) (err error) {
	database, ok := opened.(*database.DB)
	if !ok {
		return fmt.Errorf("unsupported database implementation %T", opened)
	}
	store, err := objectstore.Open(configuration.Storage.OverlayStorePath, database)
	if err != nil {
		return fmt.Errorf("initialize object store: %w", err)
	}
	defer func() {
		err = errors.Join(err, store.Close())
	}()

	repository := namespace.NewRepository(database)
	mounted, err := mount(ctx, configuration.FUSE, repository, store)
	if err != nil {
		return err
	}
	serveErr := serveControl(
		ctx,
		configuration.Control.SocketPath,
		configuration.Control.SocketMode,
		configuration.Control.AdministratorUID,
		projectionbatch.NewService(repository),
		library.NewService(repository),
		projection.NewService(repository),
	)
	return errors.Join(serveErr, mounted.Unmount())
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
