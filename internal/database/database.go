package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const DefaultLockPath = "/run/scrapefs/scrapefsd.lock"

var (
	ErrAlreadyRunning           = errors.New("scrapefsd is already running")
	ErrUnsupportedSchemaVersion = errors.New("unsupported database schema version")
)

type Options struct {
	LockPath string
}

type DB struct {
	pool           *sql.DB
	conn           *sql.Conn
	directory      *os.File
	lock           *os.File
	connectionGate chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

func Open(ctx context.Context, databasePath string) (*DB, error) {
	return OpenWithOptions(ctx, databasePath, Options{})
}

func OpenWithOptions(ctx context.Context, databasePath string, options Options) (*DB, error) {
	return open(ctx, databasePath, options, defaultMigrations)
}

func open(ctx context.Context, databasePath string, options Options, migrations []migration) (_ *DB, err error) {
	return openWithPrepareHook(ctx, databasePath, options, migrations, nil)
}

func openWithPrepareHook(
	ctx context.Context,
	databasePath string,
	options Options,
	migrations []migration,
	afterPrepare func() error,
) (_ *DB, err error) {
	if options.LockPath == "" {
		options.LockPath = DefaultLockPath
	}

	lock, err := acquireLock(options.LockPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, releaseLock(lock))
		}
	}()

	prepared, err := prepareDatabase(databasePath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.Close())
		}
	}()
	if afterPrepare != nil {
		if err := afterPrepare(); err != nil {
			return nil, fmt.Errorf("after database preparation: %w", err)
		}
	}

	pool, err := sql.Open("sqlite", sqliteDSN(prepared.sqlitePath()))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %q: %w", databasePath, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, pool.Close())
		}
	}()

	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)

	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect SQLite database %q: %w", databasePath, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, conn.Close())
		}
	}()

	version, err := readSchemaVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := validateSchemaVersion(version, migrations); err != nil {
		return nil, err
	}
	if err := configurePragmas(ctx, conn); err != nil {
		return nil, err
	}
	if err := verifyPragmas(ctx, conn); err != nil {
		return nil, err
	}
	if err := applyMigrations(ctx, conn, migrations, version); err != nil {
		return nil, err
	}

	connectionGate := make(chan struct{}, 1)
	connectionGate <- struct{}{}
	return &DB{
		pool:           pool,
		conn:           conn,
		directory:      prepared.directory,
		lock:           lock,
		connectionGate: connectionGate,
	}, nil
}

func (database *DB) UseConnection(
	ctx context.Context,
	use func(*sql.Conn) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-database.connectionGate:
	}
	if err := ctx.Err(); err != nil {
		database.connectionGate <- struct{}{}
		return err
	}
	defer func() {
		database.connectionGate <- struct{}{}
	}()
	return use(database.conn)
}

func (database *DB) Close() error {
	database.closeOnce.Do(func() {
		<-database.connectionGate
		defer func() {
			database.connectionGate <- struct{}{}
		}()
		database.closeErr = errors.Join(
			database.conn.Close(),
			database.pool.Close(),
			database.directory.Close(),
			releaseLock(database.lock),
		)
	})
	return database.closeErr
}

func sqliteDSN(databasePath string) string {
	dsn := &url.URL{Scheme: "file", Path: databasePath}
	query := dsn.Query()
	query.Set("mode", "rw")
	query.Set("_txlock", "immediate")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

type pragmaConnection interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func configurePragmas(ctx context.Context, conn pragmaConnection) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply %s: %w", statement, err)
		}
	}

	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("apply PRAGMA journal_mode = WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("incompatible PRAGMA journal_mode: got %q, want WAL", journalMode)
	}
	return nil
}

func verifyPragmas(ctx context.Context, database pragmaConnection) error {
	checks := []struct {
		name string
		want int
	}{
		{name: "foreign_keys", want: 1},
		{name: "synchronous", want: 2},
		{name: "busy_timeout", want: 5000},
	}
	for _, check := range checks {
		var got int
		if err := database.QueryRowContext(ctx, "PRAGMA "+check.name).Scan(&got); err != nil {
			return fmt.Errorf("verify PRAGMA %s: %w", check.name, err)
		}
		if got != check.want {
			return fmt.Errorf("incompatible PRAGMA %s: got %d, want %d", check.name, got, check.want)
		}
	}

	var journalMode string
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("verify PRAGMA journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("incompatible PRAGMA journal_mode: got %q, want WAL", journalMode)
	}
	return nil
}
