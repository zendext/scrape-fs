package database

import (
	"context"
	"database/sql"
	"fmt"
)

func addStableSourceIdentity(ctx context.Context, transaction *sql.Tx) error {
	if _, err := transaction.ExecContext(ctx, stableSourceIdentitySchema); err != nil {
		return fmt.Errorf("add stable Projection source identity: %w", err)
	}
	return nil
}

const stableSourceIdentitySchema = `
ALTER TABLE nodes ADD COLUMN source_filesystem_type INTEGER;
ALTER TABLE nodes ADD COLUMN source_filesystem_id_0 INTEGER;
ALTER TABLE nodes ADD COLUMN source_filesystem_id_1 INTEGER CHECK (
	(
		source_filesystem_type IS NULL
		AND source_filesystem_id_0 IS NULL
		AND source_filesystem_id_1 IS NULL
	)
	OR (
		kind = 'PROJECTION_FILE'
		AND source_filesystem_type IS NOT NULL
		AND source_filesystem_id_0 IS NOT NULL
		AND source_filesystem_id_1 IS NOT NULL
	)
);

ALTER TABLE projection_batch_entries ADD COLUMN source_filesystem_type INTEGER;
ALTER TABLE projection_batch_entries ADD COLUMN source_filesystem_id_0 INTEGER;
ALTER TABLE projection_batch_entries ADD COLUMN source_filesystem_id_1 INTEGER CHECK (
	(
		source_filesystem_type IS NULL
		AND source_filesystem_id_0 IS NULL
		AND source_filesystem_id_1 IS NULL
	)
	OR (
		source_filesystem_type IS NOT NULL
		AND source_filesystem_id_0 IS NOT NULL
		AND source_filesystem_id_1 IS NOT NULL
	)
);
`
