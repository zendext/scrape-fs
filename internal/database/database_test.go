package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

func TestOpenInitializesDatabase(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	database, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	assertPathMode(t, filepath.Dir(options.LockPath), 0o755)
	assertPathMode(t, options.LockPath, 0o600)
	assertPathMode(t, filepath.Dir(databasePath), 0o700)
	assertPathMode(t, databasePath, 0o600)

	assertPragmaInt(t, database.conn, "foreign_keys", 1)
	assertPragmaText(t, database.conn, "journal_mode", "wal")
	assertPragmaInt(t, database.conn, "synchronous", 2)
	assertPragmaInt(t, database.conn, "busy_timeout", 5000)
	assertPragmaInt(t, database.conn, "user_version", currentVersion)

	if got := database.pool.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", got)
	}
}

func TestOpenCurrentDatabaseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	first, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	if _, err := first.conn.ExecContext(context.Background(), `
		CREATE TABLE marker (value TEXT NOT NULL);
		INSERT INTO marker (value) VALUES ('preserved');
	`); err != nil {
		t.Fatalf("create marker: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer second.Close()

	var value string
	if err := second.conn.QueryRowContext(
		context.Background(),
		"SELECT value FROM marker",
	).Scan(&value); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if value != "preserved" {
		t.Errorf("marker value = %q, want preserved", value)
	}
	assertPragmaInt(t, second.conn, "user_version", currentVersion)
}

func TestOpenRejectsConcurrentDaemonBeforeDatabaseCreation(t *testing.T) {
	root := t.TempDir()
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	firstPath := filepath.Join(root, "first", "scrapefs.db")
	secondPath := filepath.Join(root, "second", "scrapefs.db")

	first, err := OpenWithOptions(context.Background(), firstPath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}

	if _, err := OpenWithOptions(context.Background(), secondPath, options); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second OpenWithOptions error = %v, want ErrAlreadyRunning", err)
	}
	if _, err := os.Stat(secondPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second database stat error = %v, want not exist", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := OpenWithOptions(context.Background(), secondPath, options)
	if err != nil {
		t.Fatalf("OpenWithOptions after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestUseConnectionDoesNotRunWithCanceledContext(t *testing.T) {
	database := openNamespaceTestDatabase(t)

	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		err := database.UseConnection(ctx, func(*Connection) error {
			called = true
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("UseConnection error = %v, want context.Canceled", err)
		}
		if called {
			t.Fatal("UseConnection called callback with canceled context")
		}
	}
}

func TestUseConnectionDoesNotRemainUsableAfterCallback(t *testing.T) {
	database := openNamespaceTestDatabase(t)
	var escaped *Connection
	var escapedRow *Row
	var escapedTransaction *Transaction

	if err := database.UseConnection(context.Background(), func(connection *Connection) error {
		escaped = connection
		escapedRow = connection.QueryRowContext(context.Background(), "SELECT 1")
		var err error
		escapedTransaction, err = connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		_, err = escapedTransaction.ExecContext(
			context.Background(),
			"INSERT INTO scrapes DEFAULT VALUES",
		)
		return err
	}); err != nil {
		t.Fatalf("UseConnection: %v", err)
	}

	var value int
	if err := escaped.QueryRowContext(
		context.Background(),
		"SELECT 1",
	).Scan(&value); !errors.Is(err, ErrConnectionLeaseExpired) {
		t.Fatalf("escaped connection error = %v, want ErrConnectionLeaseExpired", err)
	}
	if err := escapedRow.Scan(&value); !errors.Is(err, ErrConnectionLeaseExpired) {
		t.Fatalf("escaped row error = %v, want ErrConnectionLeaseExpired", err)
	}
	if _, err := escapedTransaction.ExecContext(
		context.Background(),
		"INSERT INTO scrapes DEFAULT VALUES",
	); !errors.Is(err, ErrConnectionLeaseExpired) {
		t.Fatalf("escaped transaction error = %v, want ErrConnectionLeaseExpired", err)
	}

	if err := database.UseConnection(context.Background(), func(connection *Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM scrapes",
		).Scan(&value)
	}); err != nil {
		t.Fatalf("UseConnection after expired lease: %v", err)
	}
	if value != 0 {
		t.Fatalf("Scrape count = %d, want rolled-back transaction", value)
	}
}

func TestFailedMigrationRollsBackAndReleasesLock(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	migrationFailure := errors.New("migration failed")
	testMigrations := []migration{
		{
			version: 1,
			up: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "CREATE TABLE retained (id INTEGER PRIMARY KEY)")
				return err
			},
		},
		{
			version: 2,
			up: func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, "CREATE TABLE rolled_back (id INTEGER PRIMARY KEY)"); err != nil {
					return err
				}
				return migrationFailure
			},
		},
	}

	if _, err := open(context.Background(), databasePath, options, testMigrations); !errors.Is(err, migrationFailure) {
		t.Fatalf("open error = %v, want migration failure", err)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()

	assertPragmaInt(t, raw, "user_version", 1)
	if !tableExists(t, raw, "retained") {
		t.Error("retained table does not exist")
	}
	if tableExists(t, raw, "rolled_back") {
		t.Error("rolled_back table exists after failed migration")
	}

	reopened, err := open(context.Background(), databasePath, options, testMigrations[:1])
	if err != nil {
		t.Fatalf("open after migration failure: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close after migration failure: %v", err)
	}
}

func TestOpenRejectsUnsupportedSchemaVersion(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	database, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	var journalMode string
	if err := raw.QueryRow("PRAGMA journal_mode = DELETE").Scan(&journalMode); err != nil {
		t.Fatalf("set DELETE journal mode: %v", err)
	}
	if journalMode != "delete" {
		t.Fatalf("journal mode = %q, want delete", journalMode)
	}
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", currentVersion+1)); err != nil {
		t.Fatalf("set future user_version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close: %v", err)
	}

	if _, err := OpenWithOptions(context.Background(), databasePath, options); !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Fatalf("OpenWithOptions error = %v, want ErrUnsupportedSchemaVersion", err)
	}

	raw, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen raw database: %v", err)
	}
	defer raw.Close()
	if err := raw.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "delete" {
		t.Errorf("journal mode after rejection = %q, want delete", journalMode)
	}
}

func TestOpenUsesValidatedDirectoryAfterAncestorReplacement(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	attackerDir := filepath.Join(root, "attacker")
	if err := os.Mkdir(attackerDir, 0o700); err != nil {
		t.Fatalf("Mkdir attacker: %v", err)
	}
	databasePath := filepath.Join(stateDir, "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	originalDir := stateDir + "-original"

	database, err := openWithPrepareHook(
		context.Background(),
		databasePath,
		options,
		defaultMigrations,
		func() error {
			if err := os.Rename(stateDir, originalDir); err != nil {
				return fmt.Errorf("rename validated directory: %w", err)
			}
			if err := os.Symlink(attackerDir, stateDir); err != nil {
				return fmt.Errorf("replace directory with symlink: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("openWithPrepareHook: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	originalDatabasePath := filepath.Join(originalDir, "scrapefs.db")
	raw, err := sql.Open("sqlite", originalDatabasePath)
	if err != nil {
		t.Fatalf("sql.Open original database: %v", err)
	}
	defer raw.Close()
	assertPragmaInt(t, raw, "user_version", currentVersion)

	attackerDatabasePath := filepath.Join(attackerDir, "scrapefs.db")
	if _, err := os.Stat(attackerDatabasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attacker database stat error = %v, want not exist", err)
	}
}

func TestOpenRejectsUnsafePaths(t *testing.T) {
	t.Run("lock symlink", func(t *testing.T) {
		root := t.TempDir()
		runDir := filepath.Join(root, "run")
		if err := os.Mkdir(runDir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		lockPath := filepath.Join(runDir, "scrapefsd.lock")
		if err := os.Symlink(target, lockPath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		databasePath := filepath.Join(root, "state", "scrapefs.db")
		if _, err := OpenWithOptions(context.Background(), databasePath, Options{LockPath: lockPath}); err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("OpenWithOptions error = %v, want lock path error", err)
		}
		if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("database stat error = %v, want not exist", err)
		}
	})

	t.Run("symlinked lock ancestor", func(t *testing.T) {
		root := t.TempDir()
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		linkDir := filepath.Join(root, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			filepath.Join(root, "state", "scrapefs.db"),
			Options{LockPath: filepath.Join(linkDir, "scrapefsd.lock")},
		)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("OpenWithOptions error = %v, want symlink error", err)
		}
	})

	t.Run("database symlink", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		databasePath := filepath.Join(stateDir, "scrapefs.db")
		if err := os.Symlink(target, databasePath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

		if _, err := OpenWithOptions(context.Background(), databasePath, options); err == nil || !strings.Contains(err.Error(), "database") {
			t.Fatalf("OpenWithOptions error = %v, want database path error", err)
		}

		if err := os.Remove(databasePath); err != nil {
			t.Fatalf("Remove symlink: %v", err)
		}
		reopened, err := OpenWithOptions(context.Background(), databasePath, options)
		if err != nil {
			t.Fatalf("OpenWithOptions after path fix: %v", err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("database non-regular file", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatalf("Mkdir state: %v", err)
		}
		databasePath := filepath.Join(stateDir, "scrapefs.db")
		if err := os.Mkdir(databasePath, 0o700); err != nil {
			t.Fatalf("Mkdir database path: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			databasePath,
			Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
		)
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("OpenWithOptions error = %v, want regular file error", err)
		}
	})

	t.Run("lock non-regular file", func(t *testing.T) {
		root := t.TempDir()
		runDir := filepath.Join(root, "run")
		if err := os.Mkdir(runDir, 0o755); err != nil {
			t.Fatalf("Mkdir run: %v", err)
		}
		lockPath := filepath.Join(runDir, "scrapefsd.lock")
		if err := os.Mkdir(lockPath, 0o700); err != nil {
			t.Fatalf("Mkdir lock path: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			filepath.Join(root, "state", "scrapefs.db"),
			Options{LockPath: lockPath},
		)
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("OpenWithOptions error = %v, want regular file error", err)
		}
	})

	t.Run("symlinked database ancestor", func(t *testing.T) {
		root := t.TempDir()
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		linkDir := filepath.Join(root, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			filepath.Join(linkDir, "scrapefs.db"),
			Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
		)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("OpenWithOptions error = %v, want symlink error", err)
		}
	})

	t.Run("database sidecar symlink", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		databasePath := filepath.Join(stateDir, "scrapefs.db")
		if err := os.WriteFile(databasePath, nil, 0o600); err != nil {
			t.Fatalf("WriteFile database: %v", err)
		}
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("WriteFile target: %v", err)
		}
		if err := os.Symlink(target, databasePath+"-wal"); err != nil {
			t.Fatalf("Symlink WAL: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			databasePath,
			Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
		)
		if err == nil || !strings.Contains(err.Error(), "sidecar") || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("OpenWithOptions error = %v, want sidecar symlink error", err)
		}
	})
}

func TestOpenRejectsInvalidExistingModes(t *testing.T) {
	t.Run("runtime directory", func(t *testing.T) {
		root := t.TempDir()
		runDir := filepath.Join(root, "run")
		if err := os.Mkdir(runDir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			filepath.Join(root, "state", "scrapefs.db"),
			Options{LockPath: filepath.Join(runDir, "scrapefsd.lock")},
		)
		if err == nil || !strings.Contains(err.Error(), "0755") {
			t.Fatalf("OpenWithOptions error = %v, want mode error", err)
		}
	})

	t.Run("database directory", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			filepath.Join(stateDir, "scrapefs.db"),
			Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
		)
		if err == nil || !strings.Contains(err.Error(), "0700") {
			t.Fatalf("OpenWithOptions error = %v, want mode error", err)
		}
	})

	t.Run("lock file", func(t *testing.T) {
		root := t.TempDir()
		runDir := filepath.Join(root, "run")
		if err := os.Mkdir(runDir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		lockPath := filepath.Join(runDir, "scrapefsd.lock")
		if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			filepath.Join(root, "state", "scrapefs.db"),
			Options{LockPath: lockPath},
		)
		if err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("OpenWithOptions error = %v, want mode error", err)
		}
	})

	t.Run("database file", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		databasePath := filepath.Join(stateDir, "scrapefs.db")
		if err := os.WriteFile(databasePath, nil, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := OpenWithOptions(
			context.Background(),
			databasePath,
			Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
		)
		if err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("OpenWithOptions error = %v, want mode error", err)
		}
	})
}

func TestValidateOwnerRejectsDifferentUID(t *testing.T) {
	stat := unix.Stat_t{Uid: uint32(os.Geteuid()) + 1}
	if err := validateOwner("database", &stat); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("validateOwner error = %v, want owner error", err)
	}
}

func assertPathMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %#o, want %#o", path, got, want)
	}
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func assertPragmaInt(t *testing.T, database rowQuerier, name string, want int) {
	t.Helper()
	var got int
	if err := database.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s = %d, want %d", name, got, want)
	}
}

func assertPragmaText(t *testing.T, database rowQuerier, name, want string) {
	t.Helper()
	var got string
	if err := database.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s = %q, want %q", name, got, want)
	}
}

func tableExists(t *testing.T, database rowQuerier, name string) bool {
	t.Helper()
	var count int
	if err := database.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?",
		name,
	).Scan(&count); err != nil {
		t.Fatalf("query table %q: %v", name, err)
	}
	return count == 1
}
