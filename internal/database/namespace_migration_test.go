package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestNamespaceMigrationInitializesStableGlobalRoot(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	first, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}

	firstRoot := readGlobalRoot(t, first.conn)
	if firstRoot.id != 1 {
		t.Errorf("global root ID = %d, want 1", firstRoot.id)
	}
	if firstRoot.parentID.Valid {
		t.Errorf("global root parent ID = %d, want NULL", firstRoot.parentID.Int64)
	}
	if firstRoot.name != "" {
		t.Errorf("global root name = %q, want empty", firstRoot.name)
	}
	if firstRoot.kind != "GLOBAL_ROOT" {
		t.Errorf("global root kind = %q, want GLOBAL_ROOT", firstRoot.kind)
	}
	if firstRoot.mode != 0o755 {
		t.Errorf("global root mode = %#o, want 0755", firstRoot.mode)
	}
	if firstRoot.uid != int64(os.Geteuid()) {
		t.Errorf("global root UID = %d, want %d", firstRoot.uid, os.Geteuid())
	}
	if firstRoot.gid != int64(os.Getegid()) {
		t.Errorf("global root GID = %d, want %d", firstRoot.gid, os.Getegid())
	}

	assertNamespaceVersion(t, first.conn, 0)
	assertTableCount(t, first.conn, "operation_log", 0)
	assertTableCount(t, first.conn, "objects", 0)
	assertTableCount(t, first.conn, "scrapes", 0)

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer second.Close()

	secondRoot := readGlobalRoot(t, second.conn)
	if secondRoot != firstRoot {
		t.Errorf("global root after reopen = %#v, want %#v", secondRoot, firstRoot)
	}
	assertNamespaceVersion(t, second.conn, 0)
	assertTableCount(t, second.conn, "operation_log", 0)
}

func TestNamespaceMigrationUpgradesVersionOneDatabase(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	versionOne, err := open(
		context.Background(),
		databasePath,
		options,
		defaultMigrations[:1],
	)
	if err != nil {
		t.Fatalf("open version 1 database: %v", err)
	}
	if err := versionOne.Close(); err != nil {
		t.Fatalf("close version 1 database: %v", err)
	}

	upgraded, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	defer upgraded.Close()

	assertPragmaInt(t, upgraded.conn, "user_version", currentVersion)
	if root := readGlobalRoot(t, upgraded.conn); root.id != 1 {
		t.Errorf("upgraded global root ID = %d, want 1", root.id)
	}
	assertNamespaceVersion(t, upgraded.conn, 0)
	assertTableCount(t, upgraded.conn, "operation_log", 0)
}

func TestNamespaceMigrationEnforcesNodeConstraints(t *testing.T) {
	t.Run("global root singleton", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			nil,
			"",
			"GLOBAL_ROOT",
			nil,
			nil,
			false,
		); err == nil {
			t.Fatal("insert second global root succeeded")
		}
	})

	t.Run("global root cannot be deleted", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		if _, err := database.conn.ExecContext(
			context.Background(),
			"DELETE FROM nodes WHERE id = 1",
		); err == nil {
			t.Fatal("delete global root succeeded")
		}
	})

	t.Run("global root identity cannot be changed", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		libraryRootID := insertLibraryRoot(t, database.conn, "library")
		if _, err := database.conn.ExecContext(context.Background(), `
			UPDATE nodes
			SET parent_id = ?,
				name = 'moved-root',
				kind = 'DIRECTORY'
			WHERE id = 1
		`, libraryRootID); err == nil {
			t.Fatal("change global root identity succeeded")
		}
	})

	t.Run("global root cannot be replaced", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		if _, err := database.conn.ExecContext(context.Background(), `
			INSERT OR REPLACE INTO nodes (
				id,
				parent_id,
				name,
				kind,
				creator_kind,
				creator_id,
				managed_directory,
				mode,
				uid,
				gid,
				atime_ns,
				mtime_ns,
				virtual_ctime_ns,
				created_at,
				updated_at
			) VALUES (1, NULL, '', 'GLOBAL_ROOT', 'system', NULL, 0, 448, 0, 0, 1, 1, 1, 1, 1)
		`); err == nil {
			t.Fatal("replace global root succeeded")
		}
		if root := readGlobalRoot(t, database.conn); root.mode != 0o755 {
			t.Errorf("global root mode = %#o, want 0755", root.mode)
		}
	})

	t.Run("duplicate sibling name", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		parentID := int64(1)
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&parentID,
			"library",
			"LIBRARY_ROOT",
			nil,
			nil,
			false,
		); err != nil {
			t.Fatalf("insert first library root: %v", err)
		}
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&parentID,
			"library",
			"LIBRARY_ROOT",
			nil,
			nil,
			false,
		); err == nil {
			t.Fatal("insert duplicate sibling succeeded")
		}
	})

	t.Run("invalid parent kind", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		parentID := int64(1)
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&parentID,
			"directory",
			"DIRECTORY",
			nil,
			nil,
			false,
		); err == nil {
			t.Fatal("insert directory under global root succeeded")
		}
	})

	t.Run("projection requires source fields", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		libraryRootID := insertLibraryRoot(t, database.conn, "library")
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&libraryRootID,
			"movie.mkv",
			"PROJECTION_FILE",
			nil,
			nil,
			false,
		); err == nil {
			t.Fatal("insert projection without source fields succeeded")
		}
	})

	t.Run("overlay requires an existing object", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		libraryRootID := insertLibraryRoot(t, database.conn, "library")
		missingObjectID := int64(42)
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&libraryRootID,
			"poster.jpg",
			"OVERLAY_FILE",
			nil,
			&missingObjectID,
			false,
		); err == nil {
			t.Fatal("insert overlay with missing object succeeded")
		}

		result, err := database.conn.ExecContext(
			context.Background(),
			"INSERT INTO objects DEFAULT VALUES",
		)
		if err != nil {
			t.Fatalf("insert object placeholder: %v", err)
		}
		objectID, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("object LastInsertId: %v", err)
		}
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&libraryRootID,
			"poster.jpg",
			"OVERLAY_FILE",
			nil,
			&objectID,
			false,
		); err != nil {
			t.Fatalf("insert overlay with existing object: %v", err)
		}
	})

	t.Run("origin Scrape requires an existing placeholder", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		libraryRootID := insertLibraryRoot(t, database.conn, "library")
		missingScrapeID := int64(42)
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&libraryRootID,
			"directory",
			"DIRECTORY",
			&missingScrapeID,
			nil,
			true,
		); err == nil {
			t.Fatal("insert node with missing Scrape succeeded")
		}

		result, err := database.conn.ExecContext(
			context.Background(),
			"INSERT INTO scrapes DEFAULT VALUES",
		)
		if err != nil {
			t.Fatalf("insert Scrape placeholder: %v", err)
		}
		scrapeID, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("Scrape LastInsertId: %v", err)
		}
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&libraryRootID,
			"directory",
			"DIRECTORY",
			&scrapeID,
			nil,
			true,
		); err != nil {
			t.Fatalf("insert node with existing Scrape: %v", err)
		}
	})

	t.Run("node IDs are not reused", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		firstID := insertLibraryRoot(t, database.conn, "first")
		if _, err := database.conn.ExecContext(
			context.Background(),
			"DELETE FROM nodes WHERE id = ?",
			firstID,
		); err != nil {
			t.Fatalf("delete first library root: %v", err)
		}
		secondID := insertLibraryRoot(t, database.conn, "second")
		if secondID <= firstID {
			t.Errorf("second node ID = %d, want greater than deleted ID %d", secondID, firstID)
		}
	})

	t.Run("directory with children cannot become a file", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		libraryRootID := insertLibraryRoot(t, database.conn, "library")
		directoryID, err := insertTestNode(
			context.Background(),
			database.conn,
			&libraryRootID,
			"directory",
			"DIRECTORY",
			nil,
			nil,
			true,
		)
		if err != nil {
			t.Fatalf("insert directory: %v", err)
		}
		if _, err := insertTestNode(
			context.Background(),
			database.conn,
			&directoryID,
			"child",
			"DIRECTORY",
			nil,
			nil,
			true,
		); err != nil {
			t.Fatalf("insert child: %v", err)
		}

		if _, err := database.conn.ExecContext(context.Background(), `
			UPDATE nodes
			SET kind = 'PROJECTION_FILE',
				source_path = '/source',
				source_device_id = 1,
				source_inode = 2,
				cached_size = 3,
				cached_atime_ns = 4,
				cached_mtime_ns = 5,
				cached_source_ctime_ns = 6,
				verified_at_ns = 7,
				managed_directory = 0,
				mode = 292
			WHERE id = ?
		`, directoryID); err == nil {
			t.Fatal("change non-empty directory to ProjectionFile succeeded")
		}
	})

	t.Run("directory cannot move into its descendant", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		libraryRootID := insertLibraryRoot(t, database.conn, "library")
		parentID, err := insertTestNode(
			context.Background(),
			database.conn,
			&libraryRootID,
			"parent",
			"DIRECTORY",
			nil,
			nil,
			true,
		)
		if err != nil {
			t.Fatalf("insert parent: %v", err)
		}
		childID, err := insertTestNode(
			context.Background(),
			database.conn,
			&parentID,
			"child",
			"DIRECTORY",
			nil,
			nil,
			true,
		)
		if err != nil {
			t.Fatalf("insert child: %v", err)
		}

		if _, err := database.conn.ExecContext(
			context.Background(),
			"UPDATE nodes SET parent_id = ? WHERE id = ?",
			childID,
			parentID,
		); err == nil {
			t.Fatal("move directory into descendant succeeded")
		}
	})

	t.Run("directory cannot be inserted as its own parent", func(t *testing.T) {
		database := openNamespaceTestDatabase(t)
		if _, err := database.conn.ExecContext(context.Background(), `
			INSERT INTO nodes (
				id,
				parent_id,
				name,
				kind,
				creator_kind,
				creator_id,
				managed_directory,
				mode,
				uid,
				gid,
				atime_ns,
				mtime_ns,
				virtual_ctime_ns,
				created_at,
				updated_at
			) VALUES (2, 2, 'self', 'DIRECTORY', 'system', NULL, 1, 493, 0, 0, 1, 1, 1, 1, 1)
		`); err == nil {
			t.Fatal("insert self-referencing directory succeeded")
		}
	})
}

func TestNamespaceMigrationEnforcesLibraryConstraints(t *testing.T) {
	database := openNamespaceTestDatabase(t)
	rootID := insertLibraryRoot(t, database.conn, "library")

	if _, err := database.conn.ExecContext(context.Background(), `
		INSERT INTO libraries (
			name,
			status,
			root_node_id,
			profile_name,
			profile_version,
			uid,
			gid,
			root_mode,
			directory_mode,
			projection_file_mode,
			overlay_file_mode,
			created_at,
			updated_at
		) VALUES (?, 'ACTIVE', ?, 'jellyfin', 1, 1000, 1000, 509, 509, 292, 436, 1, 1)
	`, "library", rootID); err != nil {
		t.Fatalf("insert active library: %v", err)
	}

	duplicateRootID := insertLibraryRoot(t, database.conn, "duplicate-root")
	if _, err := database.conn.ExecContext(context.Background(), `
		INSERT INTO libraries (
			name,
			status,
			root_node_id,
			profile_name,
			profile_version,
			uid,
			gid,
			root_mode,
			directory_mode,
			projection_file_mode,
			overlay_file_mode,
			created_at,
			updated_at
		) VALUES (?, 'ACTIVE', ?, 'jellyfin', 1, 1000, 1000, 509, 509, 292, 436, 1, 1)
	`, "library", duplicateRootID); err == nil {
		t.Fatal("insert duplicate active library name succeeded")
	}

	if _, err := database.conn.ExecContext(context.Background(), `
		INSERT INTO libraries (
			name,
			status,
			root_node_id,
			profile_name,
			profile_version,
			uid,
			gid,
			root_mode,
			directory_mode,
			projection_file_mode,
			overlay_file_mode,
			created_at,
			updated_at
		) VALUES ('retired', 'RETIRED', ?, 'jellyfin', 1, 1000, 1000, 509, 509, 292, 436, 1, 1)
	`, duplicateRootID); err == nil {
		t.Fatal("insert retired library with root node succeeded")
	}
}

func TestNamespaceMigrationEnforcesOperationDetailsShape(t *testing.T) {
	database := openNamespaceTestDatabase(t)
	for _, details := range []string{
		"null",
		"{}",
		"[]",
		`{"paths":[],"node_kinds":[]}`,
		`{"paths":["library"],"node_kinds":[]}`,
		`{"paths":[""],"node_kinds":["LIBRARY_ROOT"]}`,
		`{"paths":["library"],"node_kinds":["INVALID"]}`,
		`{"paths":[1],"node_kinds":["LIBRARY_ROOT"]}`,
	} {
		if _, err := database.conn.ExecContext(context.Background(), `
			INSERT INTO operation_log (
				operation,
				actor_kind,
				actor_id,
				scrape_id,
				namespace_version,
				details_json,
				created_at
			) VALUES ('test', 'system', NULL, NULL, 0, ?, 1)
		`, details); err == nil {
			t.Errorf("insert operation details %s succeeded", details)
		}
	}

	if _, err := database.conn.ExecContext(context.Background(), `
		INSERT INTO operation_log (
			operation,
			actor_kind,
			actor_id,
			scrape_id,
			namespace_version,
			details_json,
			created_at
		) VALUES (
			'test',
			'system',
			NULL,
			NULL,
			0,
			'{"paths":["library"],"node_kinds":["LIBRARY_ROOT"]}',
			1
		)
	`); err != nil {
		t.Fatalf("insert valid operation details: %v", err)
	}
}

type globalRootRecord struct {
	id       int64
	parentID sql.NullInt64
	name     string
	kind     string
	mode     int64
	uid      int64
	gid      int64
	created  int64
	updated  int64
}

func readGlobalRoot(t *testing.T, database interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) globalRootRecord {
	t.Helper()

	var record globalRootRecord
	if err := database.QueryRowContext(context.Background(), `
		SELECT id, parent_id, name, kind, mode, uid, gid, created_at, updated_at
		FROM nodes
		WHERE kind = 'GLOBAL_ROOT'
	`).Scan(
		&record.id,
		&record.parentID,
		&record.name,
		&record.kind,
		&record.mode,
		&record.uid,
		&record.gid,
		&record.created,
		&record.updated,
	); err != nil {
		t.Fatalf("read global root: %v", err)
	}
	return record
}

func openNamespaceTestDatabase(t *testing.T) *DB {
	t.Helper()

	root := t.TempDir()
	database, err := OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return database
}

func insertLibraryRoot(t *testing.T, database interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, name string) int64 {
	t.Helper()

	parentID := int64(1)
	id, err := insertTestNode(
		context.Background(),
		database,
		&parentID,
		name,
		"LIBRARY_ROOT",
		nil,
		nil,
		false,
	)
	if err != nil {
		t.Fatalf("insert library root %q: %v", name, err)
	}
	return id
}

func insertTestNode(
	ctx context.Context,
	database interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	},
	parentID *int64,
	name string,
	kind string,
	originScrapeID *int64,
	objectID *int64,
	managed bool,
) (int64, error) {
	result, err := database.ExecContext(ctx, `
		INSERT INTO nodes (
			parent_id,
			name,
			kind,
			source_path,
			source_device_id,
			source_inode,
			cached_size,
			cached_atime_ns,
			cached_mtime_ns,
			cached_source_ctime_ns,
			verified_at_ns,
			object_id,
			origin_scrape_id,
			creator_kind,
			creator_id,
			managed_directory,
			mode,
			uid,
			gid,
			atime_ns,
			mtime_ns,
			virtual_ctime_ns,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'system', NULL, ?, 493, 0, 0, 1, 1, 1, 1, 1)
	`,
		parentID,
		name,
		kind,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		objectID,
		originScrapeID,
		managed,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func assertNamespaceVersion(t *testing.T, database interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, want int64) {
	t.Helper()

	var version int64
	if err := database.QueryRowContext(
		context.Background(),
		"SELECT version FROM namespace_state WHERE singleton = 1",
	).Scan(&version); err != nil {
		t.Fatalf("read namespace version: %v", err)
	}
	if version != want {
		t.Errorf("namespace version = %d, want %d", version, want)
	}
}

func assertTableCount(t *testing.T, database interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, table string, want int64) {
	t.Helper()

	var count int64
	if err := database.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM "+table,
	).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != want {
		t.Errorf("%s count = %d, want %d", table, count, want)
	}
}
