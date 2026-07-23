package database

import (
	"context"
	"database/sql"
	"fmt"
)

func createProjectionBatchSchema(ctx context.Context, transaction *sql.Tx) error {
	if _, err := transaction.ExecContext(ctx, projectionBatchSchema); err != nil {
		return fmt.Errorf("create Projection Batch schema: %w", err)
	}
	return nil
}

const projectionBatchSchema = `
ALTER TABLE scrapes ADD COLUMN library_id INTEGER REFERENCES libraries(id) ON DELETE RESTRICT;
ALTER TABLE scrapes ADD COLUMN profile_name TEXT CHECK (profile_name IS NULL OR profile_name <> '');
ALTER TABLE scrapes ADD COLUMN profile_version INTEGER CHECK (
	profile_version IS NULL OR profile_version > 0
);
ALTER TABLE scrapes ADD COLUMN digest TEXT CHECK (
	digest IS NULL
	OR (
		length(digest) = 71
		AND digest GLOB 'sha256:[0-9a-f]*'
		AND substr(digest, 8) NOT GLOB '*[^0-9a-f]*'
	)
);
ALTER TABLE scrapes ADD COLUMN status TEXT CHECK (
	status IS NULL OR status IN ('applied', 'undone')
);
ALTER TABLE scrapes ADD COLUMN applied_by_uid INTEGER CHECK (
	applied_by_uid IS NULL OR applied_by_uid BETWEEN 0 AND 4294967295
);
ALTER TABLE scrapes ADD COLUMN applied_by_gid INTEGER CHECK (
	applied_by_gid IS NULL OR applied_by_gid BETWEEN 0 AND 4294967295
);
ALTER TABLE scrapes ADD COLUMN applied_at INTEGER;
ALTER TABLE scrapes ADD COLUMN undone_by_uid INTEGER CHECK (
	undone_by_uid IS NULL OR undone_by_uid BETWEEN 0 AND 4294967295
);
ALTER TABLE scrapes ADD COLUMN undone_by_gid INTEGER CHECK (
	undone_by_gid IS NULL OR undone_by_gid BETWEEN 0 AND 4294967295
);
ALTER TABLE scrapes ADD COLUMN undone_at INTEGER;

CREATE INDEX scrapes_status_applied_at
ON scrapes(status, applied_at, id);

CREATE TABLE projection_batch_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	scrape_id INTEGER NOT NULL REFERENCES scrapes(id) ON DELETE RESTRICT,
	position INTEGER NOT NULL CHECK (position >= 0),
	source_path TEXT NOT NULL CHECK (
		substr(source_path, 1, 1) = '/'
		AND instr(source_path, char(0)) = 0
	),
	source_device_id INTEGER NOT NULL CHECK (source_device_id >= 0),
	source_inode INTEGER NOT NULL CHECK (source_inode >= 0),
	cached_size INTEGER NOT NULL CHECK (cached_size >= 0),
	cached_atime_ns INTEGER NOT NULL,
	cached_mtime_ns INTEGER NOT NULL,
	cached_source_ctime_ns INTEGER NOT NULL,
	verified_at_ns INTEGER NOT NULL,
	target TEXT NOT NULL CHECK (
		target <> ''
		AND substr(target, 1, 1) <> '/'
		AND instr(target, char(0)) = 0
	),
	projection_node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
	UNIQUE(scrape_id, position),
	UNIQUE(scrape_id, source_path),
	UNIQUE(scrape_id, target)
) STRICT;

CREATE INDEX projection_batch_entries_scrape_id
ON projection_batch_entries(scrape_id, position);

CREATE INDEX projection_batch_entries_projection_node_id
ON projection_batch_entries(projection_node_id);
`
