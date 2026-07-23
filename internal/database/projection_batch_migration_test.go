package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestProjectionBatchMigrationExtendsLegacyScrapes(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	versionTwo, err := open(
		context.Background(),
		databasePath,
		options,
		defaultMigrations[:2],
	)
	if err != nil {
		t.Fatalf("open version 2 database: %v", err)
	}
	var legacyID int64
	if err := versionTwo.UseConnection(context.Background(), func(connection *Connection) error {
		result, err := connection.lease.connection.ExecContext(
			context.Background(),
			"INSERT INTO scrapes DEFAULT VALUES",
		)
		if err != nil {
			return err
		}
		legacyID, err = result.LastInsertId()
		return err
	}); err != nil {
		t.Fatalf("insert legacy Scrape row: %v", err)
	}
	if err := versionTwo.Close(); err != nil {
		t.Fatalf("close version 2 database: %v", err)
	}

	upgraded, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	defer upgraded.Close()

	assertPragmaInt(t, upgraded.conn, "user_version", currentVersion)
	if !tableExists(t, upgraded.conn, "projection_batch_entries") {
		t.Fatal("projection_batch_entries table does not exist")
	}

	var status sql.NullString
	var digest sql.NullString
	if err := upgraded.conn.QueryRowContext(context.Background(), `
		SELECT status, digest
		FROM scrapes
		WHERE id = ?
	`, legacyID).Scan(&status, &digest); err != nil {
		t.Fatalf("read upgraded legacy row: %v", err)
	}
	if status.Valid || digest.Valid {
		t.Errorf("legacy row provenance = status %v digest %v, want NULL fields", status, digest)
	}
}

func TestProjectionBatchMigrationEnforcesEntryReferencesAndOrder(t *testing.T) {
	database := openNamespaceTestDatabase(t)
	ctx := context.Background()

	if _, err := database.conn.ExecContext(ctx, `
		INSERT INTO projection_batch_entries (
			scrape_id,
			position,
			source_path,
			source_device_id,
			source_inode,
			cached_size,
			cached_atime_ns,
			cached_mtime_ns,
			cached_source_ctime_ns,
			verified_at_ns,
			target
		) VALUES (42, 0, '/srv/media/Dune.mkv', 1, 2, 3, 4, 5, 6, 7, 'Movies/Dune.mkv')
	`); err == nil {
		t.Fatal("insert entry with missing batch succeeded")
	}

	result, err := database.conn.ExecContext(ctx, "INSERT INTO scrapes DEFAULT VALUES")
	if err != nil {
		t.Fatalf("insert legacy Scrape row: %v", err)
	}
	scrapeID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("Scrape LastInsertId: %v", err)
	}
	insert := func(position int64) error {
		_, err := database.conn.ExecContext(ctx, `
			INSERT INTO projection_batch_entries (
				scrape_id,
				position,
				source_path,
				source_device_id,
				source_inode,
				cached_size,
				cached_atime_ns,
				cached_mtime_ns,
				cached_source_ctime_ns,
				verified_at_ns,
				target
			) VALUES (?, ?, '/srv/media/Dune.mkv', 1, 2, 3, 4, 5, 6, 7, 'Movies/Dune.mkv')
		`, scrapeID, position)
		return err
	}
	if err := insert(0); err != nil {
		t.Fatalf("insert first entry: %v", err)
	}
	if err := insert(0); err == nil {
		t.Fatal("insert duplicate entry position succeeded")
	}
}
