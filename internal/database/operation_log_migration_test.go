package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOperationLogMigrationAllowsEmptyBatchUndoDetails(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	versionFour, err := open(
		context.Background(),
		databasePath,
		options,
		defaultMigrations[:4],
	)
	if err != nil {
		t.Fatalf("open version 4 database: %v", err)
	}
	var batchID int64
	var legacyLogID int64
	if err := versionFour.UseConnection(context.Background(), func(connection *Connection) error {
		batch, err := connection.lease.connection.ExecContext(
			context.Background(),
			"INSERT INTO scrapes DEFAULT VALUES",
		)
		if err != nil {
			return err
		}
		batchID, err = batch.LastInsertId()
		if err != nil {
			return err
		}
		log, err := connection.lease.connection.ExecContext(context.Background(), `
			INSERT INTO operation_log (
				operation,
				actor_kind,
				actor_id,
				scrape_id,
				namespace_version,
				details_json,
				created_at
			) VALUES (
				'projection.batch.apply',
				'system',
				NULL,
				?,
				1,
				'{"paths":["media:/Movies/Dune.mkv"],"node_kinds":["PROJECTION_FILE"]}',
				1
			)
		`, batchID)
		if err != nil {
			return err
		}
		legacyLogID, err = log.LastInsertId()
		return err
	}); err != nil {
		t.Fatalf("create version 4 operation log: %v", err)
	}
	if err := versionFour.Close(); err != nil {
		t.Fatalf("close version 4 database: %v", err)
	}

	upgraded, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	defer upgraded.Close()

	assertPragmaInt(t, upgraded.conn, "user_version", currentVersion)
	var operation string
	if err := upgraded.conn.QueryRowContext(context.Background(), `
		SELECT operation
		FROM operation_log
		WHERE id = ?
	`, legacyLogID).Scan(&operation); err != nil {
		t.Fatalf("read migrated operation log: %v", err)
	}
	if operation != "projection.batch.apply" {
		t.Errorf("migrated operation = %q, want projection.batch.apply", operation)
	}

	result, err := upgraded.conn.ExecContext(context.Background(), `
		INSERT INTO operation_log (
			operation,
			actor_kind,
			actor_id,
			scrape_id,
			namespace_version,
			details_json,
			created_at
		) VALUES (
			'projection.batch.undo',
			'system',
			NULL,
			?,
			2,
			'{"paths":[],"node_kinds":[]}',
			2
		)
	`, batchID)
	if err != nil {
		t.Fatalf("insert empty Batch undo details: %v", err)
	}
	undoLogID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("Batch undo LastInsertId: %v", err)
	}
	if undoLogID <= legacyLogID {
		t.Errorf("Batch undo log ID = %d, want greater than %d", undoLogID, legacyLogID)
	}

	for name, test := range map[string]struct {
		statement string
		arguments []any
	}{
		"non-undo operation": {
			statement: `
			INSERT INTO operation_log (
				operation, actor_kind, actor_id, scrape_id,
				namespace_version, details_json, created_at
			) VALUES (
				'projection.remove', 'system', NULL, ?, 3,
				'{"paths":[],"node_kinds":[]}', 3
			)
		`,
			arguments: []any{batchID},
		},
		"undo without Batch": {
			statement: `
			INSERT INTO operation_log (
				operation, actor_kind, actor_id, scrape_id,
				namespace_version, details_json, created_at
			) VALUES (
				'projection.batch.undo', 'system', NULL, NULL, 3,
				'{"paths":[],"node_kinds":[]}', 3
			)
		`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := upgraded.conn.ExecContext(
				context.Background(),
				test.statement,
				test.arguments...,
			); err == nil {
				t.Fatal("invalid empty operation details succeeded")
			}
		})
	}
}
