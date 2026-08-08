package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestStableSourceIdentityMigrationPreservesLegacyProjectionData(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	versionFive, err := open(ctx, databasePath, options, defaultMigrations[:5])
	if err != nil {
		t.Fatalf("open version 5 database: %v", err)
	}
	var projectionID int64
	var batchID int64
	if err := versionFive.UseConnection(ctx, func(connection *Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		rootResult, err := transaction.ExecContext(ctx, `
			INSERT INTO nodes (
				parent_id, name, kind, creator_kind, creator_id,
				managed_directory, mode, uid, gid, atime_ns, mtime_ns,
				virtual_ctime_ns, created_at, updated_at
			) VALUES (1, 'media', 'LIBRARY_ROOT', 'system', NULL, 0, 509, 1000, 1000, 1, 1, 1, 1, 1)
		`)
		if err != nil {
			return err
		}
		rootID, err := rootResult.LastInsertId()
		if err != nil {
			return err
		}
		libraryResult, err := transaction.ExecContext(ctx, `
			INSERT INTO libraries (
				name, status, root_node_id, profile_name, profile_version,
				uid, gid, root_mode, directory_mode, projection_file_mode,
				overlay_file_mode, created_at, updated_at
			) VALUES ('media', 'ACTIVE', ?, 'jellyfin', 1, 1000, 1000, 509, 509, 292, 436, 1, 1)
		`, rootID)
		if err != nil {
			return err
		}
		libraryID, err := libraryResult.LastInsertId()
		if err != nil {
			return err
		}
		batchResult, err := transaction.ExecContext(ctx, `
			INSERT INTO scrapes (
				library_id, profile_name, profile_version, digest, status,
				applied_by_uid, applied_by_gid, applied_at
			) VALUES (?, 'jellyfin', 1, ?, 'applied', 1000, 1001, 6)
		`, libraryID, "sha256:"+strings.Repeat("a", 64))
		if err != nil {
			return err
		}
		batchID, err = batchResult.LastInsertId()
		if err != nil {
			return err
		}
		projectionResult, err := transaction.ExecContext(ctx, `
			INSERT INTO nodes (
				parent_id, name, kind, source_path, source_device_id, source_inode,
				cached_size, cached_atime_ns, cached_mtime_ns, cached_source_ctime_ns,
				verified_at_ns, origin_scrape_id, creator_kind, creator_id, managed_directory, mode,
				uid, gid, atime_ns, mtime_ns, virtual_ctime_ns, created_at, updated_at
			) VALUES (?, 'Dune.mkv', 'PROJECTION_FILE', '/srv/Dune.mkv', 47, 164144,
				123, 2, 3, 4, 5, ?, 'system', NULL, 0, 292, 1000, 1000, 2, 3, 4, 1, 1)
		`, rootID, batchID)
		if err != nil {
			return err
		}
		projectionID, err = projectionResult.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO projection_batch_entries (
				scrape_id, position, source_path, source_device_id, source_inode,
				cached_size, cached_atime_ns, cached_mtime_ns, cached_source_ctime_ns,
				verified_at_ns, target, projection_node_id
			) VALUES (?, 0, '/srv/Dune.mkv', 47, 164144, 123, 2, 3, 4, 5, 'Dune.mkv', ?)
		`, batchID, projectionID); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("create version 5 Projection: %v", err)
	}
	if err := versionFive.Close(); err != nil {
		t.Fatalf("close version 5 database: %v", err)
	}

	upgraded, err := OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	defer upgraded.Close()

	assertPragmaInt(t, upgraded.conn, "user_version", currentVersion)
	var path string
	var deviceID int64
	var inode int64
	var filesystemType sql.NullInt64
	var filesystemID0 sql.NullInt64
	var filesystemID1 sql.NullInt64
	if err := upgraded.conn.QueryRowContext(ctx, `
		SELECT source_path, source_device_id, source_inode,
			source_filesystem_type, source_filesystem_id_0, source_filesystem_id_1
		FROM nodes
		WHERE id = ?
	`, projectionID).Scan(
		&path,
		&deviceID,
		&inode,
		&filesystemType,
		&filesystemID0,
		&filesystemID1,
	); err != nil {
		t.Fatalf("read upgraded Projection: %v", err)
	}
	if path != "/srv/Dune.mkv" || deviceID != 47 || inode != 164144 {
		t.Errorf("legacy identity = %q/%d/%d", path, deviceID, inode)
	}
	if filesystemType.Valid || filesystemID0.Valid || filesystemID1.Valid {
		t.Errorf("legacy stable filesystem identity = %v/%v/%v, want NULL", filesystemType, filesystemID0, filesystemID1)
	}

	var position int64
	var batchPath string
	var batchDeviceID int64
	var batchInode int64
	var batchSize int64
	var batchAtimeNS int64
	var batchMtimeNS int64
	var batchCtimeNS int64
	var batchVerifiedAtNS int64
	var target string
	var batchProjectionID int64
	filesystemType = sql.NullInt64{}
	filesystemID0 = sql.NullInt64{}
	filesystemID1 = sql.NullInt64{}
	if err := upgraded.conn.QueryRowContext(ctx, `
		SELECT position, source_path, source_device_id, source_inode,
			cached_size, cached_atime_ns, cached_mtime_ns, cached_source_ctime_ns,
			verified_at_ns, target, projection_node_id,
			source_filesystem_type, source_filesystem_id_0, source_filesystem_id_1
		FROM projection_batch_entries
		WHERE scrape_id = ?
	`, batchID).Scan(
		&position,
		&batchPath,
		&batchDeviceID,
		&batchInode,
		&batchSize,
		&batchAtimeNS,
		&batchMtimeNS,
		&batchCtimeNS,
		&batchVerifiedAtNS,
		&target,
		&batchProjectionID,
		&filesystemType,
		&filesystemID0,
		&filesystemID1,
	); err != nil {
		t.Fatalf("read upgraded Projection Batch entry: %v", err)
	}
	if position != 0 || batchPath != "/srv/Dune.mkv" || batchDeviceID != 47 ||
		batchInode != 164144 || batchSize != 123 || batchAtimeNS != 2 ||
		batchMtimeNS != 3 || batchCtimeNS != 4 || batchVerifiedAtNS != 5 ||
		target != "Dune.mkv" || batchProjectionID != projectionID {
		t.Errorf(
			"legacy Projection Batch provenance = %d/%q/%d/%d/%d/%d/%d/%d/%d/%q/%d",
			position,
			batchPath,
			batchDeviceID,
			batchInode,
			batchSize,
			batchAtimeNS,
			batchMtimeNS,
			batchCtimeNS,
			batchVerifiedAtNS,
			target,
			batchProjectionID,
		)
	}
	if filesystemType.Valid || filesystemID0.Valid || filesystemID1.Valid {
		t.Errorf(
			"legacy Projection Batch filesystem identity = %v/%v/%v, want NULL",
			filesystemType,
			filesystemID0,
			filesystemID1,
		)
	}
}
