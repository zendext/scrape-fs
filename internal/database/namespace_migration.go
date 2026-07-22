package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

func createNamespaceSchema(ctx context.Context, transaction *sql.Tx) error {
	if _, err := transaction.ExecContext(ctx, namespaceSchema); err != nil {
		return fmt.Errorf("create namespace schema: %w", err)
	}

	now := time.Now().UTC().UnixNano()
	if _, err := transaction.ExecContext(ctx, `
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
		) VALUES (1, NULL, '', 'GLOBAL_ROOT', 'system', NULL, 0, ?, ?, ?, ?, ?, ?, ?, ?);

		INSERT INTO namespace_state (singleton, version) VALUES (1, 0);
	`, 0o755, os.Geteuid(), os.Getegid(), now, now, now, now, now); err != nil {
		return fmt.Errorf("initialize namespace: %w", err)
	}
	return nil
}

const namespaceSchema = `
CREATE TABLE objects (
	id INTEGER PRIMARY KEY AUTOINCREMENT
) STRICT;

CREATE TABLE scrapes (
	id INTEGER PRIMARY KEY AUTOINCREMENT
) STRICT;

CREATE TABLE nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	parent_id INTEGER REFERENCES nodes(id) ON DELETE RESTRICT,
	name TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (
		kind IN (
			'GLOBAL_ROOT',
			'LIBRARY_ROOT',
			'DIRECTORY',
			'PROJECTION_FILE',
			'OVERLAY_FILE'
		)
	),
	source_path TEXT,
	source_device_id INTEGER,
	source_inode INTEGER,
	cached_size INTEGER,
	cached_atime_ns INTEGER,
	cached_mtime_ns INTEGER,
	cached_source_ctime_ns INTEGER,
	verified_at_ns INTEGER,
	object_id INTEGER REFERENCES objects(id) ON DELETE RESTRICT,
	origin_scrape_id INTEGER REFERENCES scrapes(id) ON DELETE SET NULL,
	creator_kind TEXT NOT NULL CHECK (
		creator_kind IN ('control', 'agent', 'fuse', 'system')
	),
	creator_id INTEGER,
	managed_directory INTEGER NOT NULL CHECK (managed_directory IN (0, 1)),
	mode INTEGER NOT NULL CHECK (mode BETWEEN 0 AND 511),
	uid INTEGER NOT NULL CHECK (uid BETWEEN 0 AND 4294967295),
	gid INTEGER NOT NULL CHECK (gid BETWEEN 0 AND 4294967295),
	atime_ns INTEGER NOT NULL,
	mtime_ns INTEGER NOT NULL,
	virtual_ctime_ns INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	CHECK (
		(kind = 'GLOBAL_ROOT' AND id = 1 AND parent_id IS NULL AND name = '')
		OR
		(
			kind <> 'GLOBAL_ROOT'
			AND parent_id IS NOT NULL
			AND name <> ''
			AND name NOT IN ('.', '..')
			AND instr(name, '/') = 0
			AND instr(name, char(0)) = 0
			AND length(CAST(name AS BLOB)) <= 255
		)
	),
	CHECK (
		(creator_kind = 'system' AND creator_id IS NULL)
		OR
		(creator_kind <> 'system' AND creator_id IS NOT NULL)
	),
	CHECK (
		(
			kind = 'PROJECTION_FILE'
			AND source_path IS NOT NULL
			AND substr(source_path, 1, 1) = '/'
			AND source_device_id IS NOT NULL
			AND source_device_id >= 0
			AND source_inode IS NOT NULL
			AND source_inode >= 0
			AND cached_size IS NOT NULL
			AND cached_size >= 0
			AND cached_atime_ns IS NOT NULL
			AND cached_mtime_ns IS NOT NULL
			AND cached_source_ctime_ns IS NOT NULL
			AND verified_at_ns IS NOT NULL
			AND object_id IS NULL
			AND managed_directory = 0
			AND (mode & 146) = 0
		)
		OR
		(
			kind = 'OVERLAY_FILE'
			AND source_path IS NULL
			AND source_device_id IS NULL
			AND source_inode IS NULL
			AND cached_size IS NULL
			AND cached_atime_ns IS NULL
			AND cached_mtime_ns IS NULL
			AND cached_source_ctime_ns IS NULL
			AND verified_at_ns IS NULL
			AND object_id IS NOT NULL
			AND managed_directory = 0
		)
		OR
		(
			kind IN ('GLOBAL_ROOT', 'LIBRARY_ROOT', 'DIRECTORY')
			AND source_path IS NULL
			AND source_device_id IS NULL
			AND source_inode IS NULL
			AND cached_size IS NULL
			AND cached_atime_ns IS NULL
			AND cached_mtime_ns IS NULL
			AND cached_source_ctime_ns IS NULL
			AND verified_at_ns IS NULL
			AND object_id IS NULL
			AND (
				kind = 'DIRECTORY'
				OR managed_directory = 0
			)
		)
	),
	CHECK (
		kind NOT IN ('GLOBAL_ROOT', 'LIBRARY_ROOT')
		OR origin_scrape_id IS NULL
	),
	UNIQUE(parent_id, name)
) STRICT;

CREATE INDEX nodes_parent_id ON nodes(parent_id);
CREATE INDEX nodes_origin_scrape_id ON nodes(origin_scrape_id);
CREATE INDEX nodes_object_id ON nodes(object_id);

CREATE TABLE libraries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL CHECK (
		name <> ''
		AND name NOT IN ('.', '..')
		AND instr(name, '/') = 0
		AND instr(name, char(0)) = 0
		AND length(CAST(name AS BLOB)) <= 255
	),
	status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'RETIRED')),
	root_node_id INTEGER UNIQUE REFERENCES nodes(id) ON DELETE SET NULL,
	profile_name TEXT NOT NULL CHECK (profile_name <> ''),
	profile_version INTEGER NOT NULL CHECK (profile_version > 0),
	uid INTEGER NOT NULL CHECK (uid BETWEEN 0 AND 4294967295),
	gid INTEGER NOT NULL CHECK (gid BETWEEN 0 AND 4294967295),
	root_mode INTEGER NOT NULL CHECK (root_mode BETWEEN 0 AND 511),
	directory_mode INTEGER NOT NULL CHECK (directory_mode BETWEEN 0 AND 511),
	projection_file_mode INTEGER NOT NULL CHECK (
		projection_file_mode BETWEEN 0 AND 511
		AND (projection_file_mode & 146) = 0
	),
	overlay_file_mode INTEGER NOT NULL CHECK (overlay_file_mode BETWEEN 0 AND 511),
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	retired_at INTEGER,
	CHECK (
		(status = 'ACTIVE' AND root_node_id IS NOT NULL AND retired_at IS NULL)
		OR
		(status = 'RETIRED' AND root_node_id IS NULL AND retired_at IS NOT NULL)
	)
) STRICT;

CREATE UNIQUE INDEX libraries_active_name
ON libraries(name)
WHERE status = 'ACTIVE';

CREATE TABLE namespace_state (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	version INTEGER NOT NULL CHECK (version >= 0)
) STRICT;

CREATE TABLE operation_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	operation TEXT NOT NULL CHECK (operation <> ''),
	actor_kind TEXT NOT NULL CHECK (
		actor_kind IN ('control', 'agent', 'fuse', 'system')
	),
	actor_id INTEGER,
	scrape_id INTEGER REFERENCES scrapes(id) ON DELETE SET NULL,
	namespace_version INTEGER NOT NULL CHECK (namespace_version >= 0),
	details_json TEXT NOT NULL CHECK (
		CASE
			WHEN json_valid(details_json) THEN
				COALESCE(
					json_type(details_json, '$.paths') = 'array'
					AND json_array_length(details_json, '$.paths') > 0
					AND json_type(details_json, '$.node_kinds') = 'array'
					AND json_array_length(details_json, '$.node_kinds') =
						json_array_length(details_json, '$.paths'),
					0
				)
			ELSE 0
		END
	),
	created_at INTEGER NOT NULL,
	CHECK (
		(actor_kind = 'system' AND actor_id IS NULL)
		OR
		(actor_kind <> 'system' AND actor_id IS NOT NULL)
	)
) STRICT;

CREATE INDEX operation_log_namespace_version
ON operation_log(namespace_version);

CREATE INDEX operation_log_scrape_id
ON operation_log(scrape_id);
`
