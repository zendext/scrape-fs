package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestObjectStoreMigrationExtendsLegacyObjects(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}

	versionThree, err := open(
		context.Background(),
		databasePath,
		options,
		defaultMigrations[:3],
	)
	if err != nil {
		t.Fatalf("open version 3 database: %v", err)
	}
	var legacyID int64
	if err := versionThree.UseConnection(context.Background(), func(connection *Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		result, err := transaction.ExecContext(
			context.Background(),
			"INSERT INTO objects DEFAULT VALUES",
		)
		if err != nil {
			return err
		}
		legacyID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("insert legacy object: %v", err)
	}
	if err := versionThree.Close(); err != nil {
		t.Fatalf("close version 3 database: %v", err)
	}

	upgraded, err := OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	defer upgraded.Close()

	assertPragmaInt(t, upgraded.conn, "user_version", currentVersion)
	var state string
	var createdAt int64
	var liveAt sql.NullInt64
	var updatedAt int64
	if err := upgraded.conn.QueryRowContext(context.Background(), `
		SELECT state, created_at_ns, live_at_ns, updated_at_ns
		FROM objects
		WHERE id = ?
	`, legacyID).Scan(&state, &createdAt, &liveAt, &updatedAt); err != nil {
		t.Fatalf("read upgraded object: %v", err)
	}
	if state != "STAGING" || createdAt != 0 || liveAt.Valid || updatedAt != 0 {
		t.Errorf(
			"upgraded object = state %q created %d live %v updated %d, want STAGING/0/NULL/0",
			state,
			createdAt,
			liveAt,
			updatedAt,
		)
	}
}

func TestObjectStoreMigrationEnforcesLifecycle(t *testing.T) {
	database := openNamespaceTestDatabase(t)
	ctx := context.Background()

	if _, err := database.conn.ExecContext(ctx, `
		INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
		VALUES ('STAGING', 10, NULL, 10)
	`); err != nil {
		t.Fatalf("insert STAGING object: %v", err)
	}
	if _, err := database.conn.ExecContext(ctx, `
		INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
		VALUES ('LIVE', 10, 20, 20)
	`); err != nil {
		t.Fatalf("insert LIVE object: %v", err)
	}

	for name, statement := range map[string]string{
		"invalid state": `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES ('UNKNOWN', 10, NULL, 10)
		`,
		"staging with live timestamp": `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES ('STAGING', 10, 20, 20)
		`,
		"live without live timestamp": `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES ('LIVE', 10, NULL, 20)
		`,
		"updated before creation": `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES ('STAGING', 20, NULL, 10)
		`,
		"live before creation": `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES ('LIVE', 20, 10, 20)
		`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := database.conn.ExecContext(ctx, statement); err == nil {
				t.Fatal("invalid object insert succeeded")
			}
		})
	}
}
