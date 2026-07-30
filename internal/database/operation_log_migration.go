package database

import (
	"context"
	"database/sql"
	"fmt"
)

func updateOperationLogDetailsSchema(ctx context.Context, transaction *sql.Tx) error {
	if _, err := transaction.ExecContext(ctx, operationLogDetailsSchema); err != nil {
		return fmt.Errorf("update operation-log details schema: %w", err)
	}
	return nil
}

const operationLogDetailsSchema = `
ALTER TABLE operation_log RENAME TO operation_log_v4;

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
					AND json_type(details_json, '$.node_kinds') = 'array'
					AND json_array_length(details_json, '$.node_kinds') =
						json_array_length(details_json, '$.paths')
					AND (
						json_array_length(details_json, '$.paths') > 0
						OR (
							operation = 'projection.batch.undo'
							AND scrape_id IS NOT NULL
						)
					),
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

INSERT INTO operation_log (
	id,
	operation,
	actor_kind,
	actor_id,
	scrape_id,
	namespace_version,
	details_json,
	created_at
)
SELECT
	id,
	operation,
	actor_kind,
	actor_id,
	scrape_id,
	namespace_version,
	details_json,
	created_at
FROM operation_log_v4;

DROP TABLE operation_log_v4;

CREATE INDEX operation_log_namespace_version
ON operation_log(namespace_version);

CREATE INDEX operation_log_scrape_id
ON operation_log(scrape_id);
`
