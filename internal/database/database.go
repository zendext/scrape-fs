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
	sql       *sql.DB
	lock      *os.File
	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, databasePath string) (*DB, error) {
	return OpenWithOptions(ctx, databasePath, Options{})
}

func OpenWithOptions(ctx context.Context, databasePath string, options Options) (*DB, error) {
	return open(ctx, databasePath, options, defaultMigrations)
}

func open(ctx context.Context, databasePath string, options Options, migrations []migration) (_ *DB, err error) {
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

	if err := prepareDatabaseFile(databasePath); err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open("sqlite", sqliteDSN(databasePath))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %q: %w", databasePath, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, sqlDB.Close())
		}
	}()

	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect SQLite database %q: %w", databasePath, err)
	}
	if err := verifyPragmas(ctx, sqlDB); err != nil {
		return nil, err
	}
	if err := applyMigrations(ctx, sqlDB, migrations); err != nil {
		return nil, err
	}

	return &DB{sql: sqlDB, lock: lock}, nil
}

func (database *DB) SQL() *sql.DB {
	return database.sql
}

func (database *DB) Close() error {
	database.closeOnce.Do(func() {
		database.closeErr = errors.Join(database.sql.Close(), releaseLock(database.lock))
	})
	return database.closeErr
}

func sqliteDSN(databasePath string) string {
	dsn := &url.URL{Scheme: "file", Path: databasePath}
	query := dsn.Query()
	query.Set("mode", "rwc")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func verifyPragmas(ctx context.Context, database *sql.DB) error {
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
