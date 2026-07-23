package database

import (
	"context"
	"database/sql"
	"fmt"
)

func createObjectStoreSchema(ctx context.Context, transaction *sql.Tx) error {
	if _, err := transaction.ExecContext(ctx, objectStoreSchema); err != nil {
		return fmt.Errorf("create object-store schema: %w", err)
	}
	return nil
}

const objectStoreSchema = `
ALTER TABLE objects ADD COLUMN state TEXT NOT NULL DEFAULT 'STAGING' CHECK (
	state IN ('STAGING', 'LIVE')
);
ALTER TABLE objects ADD COLUMN created_at_ns INTEGER NOT NULL DEFAULT 0 CHECK (
	created_at_ns >= 0
);
ALTER TABLE objects ADD COLUMN live_at_ns INTEGER CHECK (
	(state = 'STAGING' AND live_at_ns IS NULL)
	OR
	(state = 'LIVE' AND live_at_ns IS NOT NULL AND live_at_ns >= created_at_ns)
);
ALTER TABLE objects ADD COLUMN updated_at_ns INTEGER NOT NULL DEFAULT 0 CHECK (
	updated_at_ns >= created_at_ns
	AND (live_at_ns IS NULL OR updated_at_ns >= live_at_ns)
);
`
