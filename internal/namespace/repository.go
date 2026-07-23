package namespace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type Repository struct {
	database *database.DB
	now      func() time.Time
}

func NewRepository(opened *database.DB) *Repository {
	return &Repository{
		database: opened,
		now:      time.Now,
	}
}

func (repository *Repository) GlobalRoot(ctx context.Context) (Node, error) {
	var node Node
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		node, err = globalRoot(ctx, connection)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return node, nil
}

func (repository *Repository) NamespaceVersion(ctx context.Context) (int64, error) {
	var version int64
	if err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		version, err = namespaceVersion(ctx, connection)
		return err
	}); err != nil {
		return 0, err
	}
	return version, nil
}

func (repository *Repository) Library(ctx context.Context, name string) (Library, error) {
	if err := validateName(name); err != nil {
		return Library{}, fmt.Errorf("%w: invalid name: %v", ErrInvalidLibrary, err)
	}

	var library Library
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		library, err = queryLibrary(
			ctx,
			connection,
			"libraries.status = 'ACTIVE' AND libraries.name = ?",
			name,
		)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return Library{}, fmt.Errorf("%w: %q", ErrLibraryNotFound, name)
	}
	if err != nil {
		return Library{}, fmt.Errorf("get Library %q: %w", name, err)
	}
	return library, nil
}

func (repository *Repository) Libraries(ctx context.Context) ([]Library, error) {
	var libraries []Library
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		cursorName := ""
		var cursorID int64
		for {
			library, err := queryLibrary(ctx, connection, `
				libraries.status = 'ACTIVE'
				AND (
					libraries.name > ?
					OR (libraries.name = ? AND libraries.id > ?)
				)
				ORDER BY libraries.name, libraries.id
				LIMIT 1
			`, cursorName, cursorName, cursorID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("list Libraries: %w", err)
			}
			libraries = append(libraries, library)
			cursorName = library.Name
			cursorID = library.ID
		}
	})
	if err != nil {
		return nil, err
	}
	return libraries, nil
}

func (repository *Repository) ProjectionBatches(
	ctx context.Context,
) ([]ProjectionBatch, error) {
	var batches []ProjectionBatch
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var cursorID int64
		for {
			batch, err := queryProjectionBatch(ctx, connection, `
				scrapes.status IN ('applied', 'undone')
				AND scrapes.id > ?
				ORDER BY scrapes.id
				LIMIT 1
			`, cursorID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("list Projection Batches: %w", err)
			}
			batches = append(batches, batch)
			cursorID = batch.ID
		}
	})
	if err != nil {
		return nil, err
	}
	return batches, nil
}

func (repository *Repository) ProjectionBatch(
	ctx context.Context,
	id int64,
) (ProjectionBatch, error) {
	if id <= 0 {
		return ProjectionBatch{}, ErrNotFound
	}

	var batch ProjectionBatch
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		batch, err = queryProjectionBatch(
			ctx,
			connection,
			"scrapes.id = ? AND scrapes.status IN ('applied', 'undone')",
			id,
		)
		return err
	})
	if err != nil {
		return ProjectionBatch{}, err
	}
	return batch, nil
}

func (repository *Repository) ProjectionBatchEntries(
	ctx context.Context,
	batchID int64,
) ([]ProjectionBatchEntry, error) {
	var entries []ProjectionBatchEntry
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		entries, err = projectionBatchEntries(ctx, connection, batchID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (repository *Repository) ProjectionBatchOperations(
	ctx context.Context,
	batchID int64,
) ([]ProjectionBatchOperation, error) {
	var operations []ProjectionBatchOperation
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		operations, err = projectionBatchOperations(ctx, connection, batchID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return operations, nil
}

func (repository *Repository) ProjectionBatchDetail(
	ctx context.Context,
	batchID int64,
) (ProjectionBatch, []ProjectionBatchEntry, []ProjectionBatchOperation, error) {
	if batchID <= 0 {
		return ProjectionBatch{}, nil, nil, ErrNotFound
	}

	var batch ProjectionBatch
	var entries []ProjectionBatchEntry
	var operations []ProjectionBatchOperation
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		batch, err = queryProjectionBatch(
			ctx,
			connection,
			"scrapes.id = ? AND scrapes.status IN ('applied', 'undone')",
			batchID,
		)
		if err != nil {
			return err
		}
		entries, err = projectionBatchEntries(ctx, connection, batchID)
		if err != nil {
			return err
		}
		operations, err = projectionBatchOperations(ctx, connection, batchID)
		return err
	})
	if err != nil {
		return ProjectionBatch{}, nil, nil, err
	}
	return batch, entries, operations, nil
}

func (repository *Repository) Lookup(
	ctx context.Context,
	libraryRootID int64,
	relativePath string,
) (Node, error) {
	components, err := ValidateRelativePath(relativePath)
	if err != nil {
		return Node{}, err
	}

	var current Node
	err = repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		current, err = lookupComponents(ctx, connection, libraryRootID, components)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return current, nil
}

func (repository *Repository) ValidateProjectionTargets(
	ctx context.Context,
	libraryName string,
	relativePaths []string,
) (Library, int64, error) {
	if err := validateName(libraryName); err != nil {
		return Library{}, 0, fmt.Errorf("%w: invalid name: %v", ErrInvalidLibrary, err)
	}
	if len(relativePaths) == 0 {
		return Library{}, 0, fmt.Errorf("%w: targets must not be empty", ErrInvalidPath)
	}
	components := make([][]string, len(relativePaths))
	for index, relativePath := range relativePaths {
		validated, err := ValidateRelativePath(relativePath)
		if err != nil {
			return Library{}, 0, err
		}
		components[index] = validated
	}

	var library Library
	var version int64
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		library, err = queryLibrary(
			ctx,
			connection,
			"libraries.status = 'ACTIVE' AND libraries.name = ?",
			libraryName,
		)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %q", ErrLibraryNotFound, libraryName)
		}
		if err != nil {
			return fmt.Errorf("get Library %q: %w", libraryName, err)
		}
		for _, targetComponents := range components {
			if err := checkTargetAvailable(
				ctx,
				connection,
				library.RootNodeID,
				targetComponents,
			); err != nil {
				return err
			}
		}
		version, err = namespaceVersion(ctx, connection)
		return err
	})
	if err != nil {
		return Library{}, 0, err
	}
	return library, version, nil
}

// Update holds exclusive database access for the callback. The callback must
// use Transaction read methods and must not call methods on a Repository.
func (repository *Repository) Update(
	ctx context.Context,
	operation Operation,
	change func(*Transaction) error,
) (_ int64, err error) {
	if err := validateOperation(operation, change); err != nil {
		return 0, err
	}
	detailsJSON, err := json.Marshal(operation.Details)
	if err != nil {
		return 0, fmt.Errorf("%w: encode details: %v", ErrInvalidOperation, err)
	}

	var version int64
	err = repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		version, err = repository.update(
			ctx,
			connection,
			operation,
			detailsJSON,
			change,
		)
		return err
	})
	return version, err
}

func (repository *Repository) useConnection(
	ctx context.Context,
	use func(*database.Connection) error,
) error {
	return repository.database.UseConnection(ctx, use)
}

func (repository *Repository) update(
	ctx context.Context,
	connection *database.Connection,
	operation Operation,
	detailsJSON []byte,
	change func(*Transaction) error,
) (_ int64, err error) {
	transaction, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin namespace transaction: %w", err)
	}
	defer transaction.Rollback()

	rollback := func(cause error) error {
		rollbackErr := transaction.Rollback()
		if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
			return cause
		}
		return errors.Join(cause, fmt.Errorf("rollback namespace transaction: %w", rollbackErr))
	}

	namespaceTransaction := &Transaction{
		transaction: transaction,
		now:         repository.now,
	}
	if err := change(namespaceTransaction); err != nil {
		return 0, rollback(err)
	}

	if !namespaceTransaction.changed {
		var version int64
		if err := transaction.QueryRowContext(
			ctx,
			"SELECT version FROM namespace_state WHERE singleton = 1",
		).Scan(&version); err != nil {
			return 0, rollback(fmt.Errorf("read namespace version: %w", err))
		}
		if err := transaction.Commit(); err != nil {
			return 0, fmt.Errorf("commit no-op namespace transaction: %w", err)
		}
		return version, nil
	}

	var version int64
	if err := transaction.QueryRowContext(ctx, `
		UPDATE namespace_state
		SET version = version + 1
		WHERE singleton = 1
		RETURNING version
	`).Scan(&version); err != nil {
		return 0, rollback(fmt.Errorf("increment namespace version: %w", err))
	}

	scrapeID := operation.ScrapeID
	if namespaceTransaction.operationScrapeID != nil {
		if scrapeID != nil && *scrapeID != *namespaceTransaction.operationScrapeID {
			return 0, rollback(fmt.Errorf(
				"%w: conflicting Projection Batch operation references",
				ErrInvalidOperation,
			))
		}
		scrapeID = namespaceTransaction.operationScrapeID
	}

	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO operation_log (
			operation,
			actor_kind,
			actor_id,
			scrape_id,
			namespace_version,
			details_json,
			created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		operation.Name,
		operation.Actor.Kind,
		operation.Actor.ID,
		scrapeID,
		version,
		string(detailsJSON),
		repository.now().UTC().UnixNano(),
	); err != nil {
		return 0, rollback(fmt.Errorf("write operation log: %w", err))
	}

	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit namespace transaction: %w", err)
	}
	return version, nil
}

type Transaction struct {
	transaction       *database.Transaction
	now               func() time.Time
	changed           bool
	operationScrapeID *int64
}

func (transaction *Transaction) GlobalRoot(ctx context.Context) (Node, error) {
	return globalRoot(ctx, transaction.transaction)
}

func (transaction *Transaction) NamespaceVersion(ctx context.Context) (int64, error) {
	return namespaceVersion(ctx, transaction.transaction)
}

func (transaction *Transaction) Lookup(
	ctx context.Context,
	libraryRootID int64,
	relativePath string,
) (Node, error) {
	components, err := ValidateRelativePath(relativePath)
	if err != nil {
		return Node{}, err
	}
	return lookupComponents(ctx, transaction.transaction, libraryRootID, components)
}

func (transaction *Transaction) CheckTargetAvailable(
	ctx context.Context,
	libraryRootID int64,
	relativePath string,
) error {
	components, err := ValidateRelativePath(relativePath)
	if err != nil {
		return err
	}
	return checkTargetAvailable(ctx, transaction.transaction, libraryRootID, components)
}

func (transaction *Transaction) ProjectionBatch(
	ctx context.Context,
	id int64,
) (ProjectionBatch, error) {
	return queryProjectionBatch(
		ctx,
		transaction.transaction,
		"scrapes.id = ? AND scrapes.status IN ('applied', 'undone')",
		id,
	)
}

func (transaction *Transaction) CreateLibrary(
	ctx context.Context,
	request CreateLibrary,
) (Library, error) {
	if err := request.Validate(); err != nil {
		return Library{}, err
	}

	globalRoot, err := transaction.GlobalRoot(ctx)
	if err != nil {
		return Library{}, err
	}
	root, err := transaction.CreateNode(ctx, CreateNode{
		ParentID: globalRoot.ID,
		Name:     request.Name,
		Kind:     LibraryRoot,
		Creator:  request.Creator,
		Attributes: Attributes{
			Mode: request.RootMode,
			UID:  request.UID,
			GID:  request.GID,
		},
	})
	if err != nil {
		return Library{}, err
	}

	now := transaction.now().UTC().UnixNano()
	result, err := transaction.transaction.ExecContext(ctx, `
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
			updated_at,
			retired_at
		) VALUES (?, 'ACTIVE', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
	`,
		request.Name,
		root.ID,
		request.ProfileName,
		request.ProfileVersion,
		request.UID,
		request.GID,
		request.RootMode.Perm(),
		request.DirectoryMode.Perm(),
		request.ProjectionMode.Perm(),
		request.OverlayMode.Perm(),
		now,
		now,
	)
	if err != nil {
		return Library{}, classifyLibraryMutationError("create Library", err)
	}
	libraryID, err := result.LastInsertId()
	if err != nil {
		return Library{}, fmt.Errorf("read created Library ID: %w", err)
	}

	library, err := queryLibrary(
		ctx,
		transaction.transaction,
		"libraries.id = ?",
		libraryID,
	)
	if err != nil {
		return Library{}, fmt.Errorf("read created Library: %w", err)
	}
	return library, nil
}

func (transaction *Transaction) CreateNode(
	ctx context.Context,
	request CreateNode,
) (Node, error) {
	if err := validateCreateNode(request); err != nil {
		return Node{}, err
	}
	if request.Kind == OverlayFile {
		return Node{}, fmt.Errorf(
			"%w: OverlayFile must be created through CreateOverlay",
			ErrInvalidNode,
		)
	}
	return transaction.createNode(ctx, request)
}

func (transaction *Transaction) createNode(
	ctx context.Context,
	request CreateNode,
) (Node, error) {
	if err := transaction.validateParent(ctx, request); err != nil {
		return Node{}, err
	}

	now := transaction.now().UTC().UnixNano()
	var sourcePath any
	var sourceDeviceID any
	var sourceInode any
	var cachedSize any
	var cachedAtimeNS any
	var cachedMtimeNS any
	var cachedSourceCtimeNS any
	var verifiedAtNS any
	if request.Source != nil {
		sourcePath = request.Source.Path
		sourceDeviceID = request.Source.DeviceID
		sourceInode = request.Source.Inode
		cachedSize = request.Source.Size
		cachedAtimeNS = request.Source.AtimeNS
		cachedMtimeNS = request.Source.MtimeNS
		cachedSourceCtimeNS = request.Source.SourceCtimeNS
		verifiedAtNS = request.Source.VerifiedAtNS
	}

	result, err := transaction.transaction.ExecContext(ctx, `
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		request.ParentID,
		request.Name,
		request.Kind,
		sourcePath,
		sourceDeviceID,
		sourceInode,
		cachedSize,
		cachedAtimeNS,
		cachedMtimeNS,
		cachedSourceCtimeNS,
		verifiedAtNS,
		request.ObjectID,
		request.OriginScrapeID,
		request.Creator.Kind,
		request.Creator.ID,
		request.ManagedDirectory,
		request.Attributes.Mode.Perm(),
		request.Attributes.UID,
		request.Attributes.GID,
		now,
		now,
		now,
		now,
		now,
	)
	if err != nil {
		return Node{}, classifyMutationError("create node", err)
	}
	nodeID, err := result.LastInsertId()
	if err != nil {
		return Node{}, fmt.Errorf("read created node ID: %w", err)
	}
	if err := transaction.touchDirectory(ctx, request.ParentID, now); err != nil {
		return Node{}, err
	}
	node, err := queryNode(ctx, transaction.transaction, "id = ?", nodeID)
	if err != nil {
		return Node{}, fmt.Errorf("read created node: %w", err)
	}
	transaction.changed = true
	return node, nil
}

func (transaction *Transaction) CreateOverlay(
	ctx context.Context,
	objectID int64,
	request CreateOverlay,
) (Node, error) {
	if objectID <= 0 {
		return Node{}, fmt.Errorf("%w: object ID must be positive", ErrInvalidObject)
	}
	objectIDCopy := objectID
	nodeRequest := CreateNode{
		ParentID:   request.ParentID,
		Name:       request.Name,
		Kind:       OverlayFile,
		ObjectID:   &objectIDCopy,
		Creator:    request.Creator,
		Attributes: request.Attributes,
	}
	if err := validateCreateNode(nodeRequest); err != nil {
		return Node{}, err
	}

	now := transaction.now().UTC().UnixNano()
	result, err := transaction.transaction.ExecContext(ctx, `
		UPDATE objects
		SET state = 'LIVE', live_at_ns = ?, updated_at_ns = ?
		WHERE id = ?
		  AND state = 'STAGING'
	`, now, now, objectID)
	if err != nil {
		return Node{}, fmt.Errorf("promote backing object %d: %w", objectID, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return Node{}, fmt.Errorf("read promoted backing object count: %w", err)
	}
	if updated != 1 {
		return Node{}, fmt.Errorf(
			"%w: object %d is missing or not STAGING",
			ErrInvalidObject,
			objectID,
		)
	}
	return transaction.createNode(ctx, nodeRequest)
}

func (transaction *Transaction) CreateProjectionBatch(
	ctx context.Context,
	request CreateProjectionBatch,
) (ProjectionBatch, error) {
	if err := validateCreateProjectionBatch(request); err != nil {
		return ProjectionBatch{}, err
	}

	result, err := transaction.transaction.ExecContext(ctx, `
		INSERT INTO scrapes (
			library_id,
			profile_name,
			profile_version,
			digest,
			status,
			applied_by_uid,
			applied_by_gid,
			applied_at,
			undone_by_uid,
			undone_by_gid,
			undone_at
		) VALUES (?, ?, ?, ?, 'applied', ?, ?, ?, NULL, NULL, NULL)
	`,
		request.LibraryID,
		request.ProfileName,
		request.ProfileVersion,
		request.Digest,
		request.AppliedByUID,
		request.AppliedByGID,
		request.AppliedAt,
	)
	if err != nil {
		return ProjectionBatch{}, fmt.Errorf("create Projection Batch: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ProjectionBatch{}, fmt.Errorf("read created Projection Batch ID: %w", err)
	}
	batch, err := queryProjectionBatch(
		ctx,
		transaction.transaction,
		"scrapes.id = ?",
		id,
	)
	if err != nil {
		return ProjectionBatch{}, fmt.Errorf("read created Projection Batch: %w", err)
	}
	return batch, nil
}

func (transaction *Transaction) CreateProjectionBatchEntry(
	ctx context.Context,
	request CreateProjectionBatchEntry,
) error {
	if err := validateCreateProjectionBatchEntry(request); err != nil {
		return err
	}
	node, err := queryNode(
		ctx,
		transaction.transaction,
		"id = ?",
		request.ProjectionNodeID,
	)
	if err != nil {
		return fmt.Errorf("read Projection Batch node: %w", err)
	}
	if node.Kind != ProjectionFile ||
		node.OriginScrapeID == nil ||
		*node.OriginScrapeID != request.BatchID ||
		node.Source == nil ||
		*node.Source != request.Source {
		return fmt.Errorf(
			"%w: Projection Batch entry does not match node %d",
			ErrInvalidNode,
			request.ProjectionNodeID,
		)
	}

	if _, err := transaction.transaction.ExecContext(ctx, `
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
			target,
			projection_node_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		request.BatchID,
		request.Position,
		request.Source.Path,
		request.Source.DeviceID,
		request.Source.Inode,
		request.Source.Size,
		request.Source.AtimeNS,
		request.Source.MtimeNS,
		request.Source.SourceCtimeNS,
		request.Source.VerifiedAtNS,
		request.Target,
		request.ProjectionNodeID,
	); err != nil {
		return fmt.Errorf("create Projection Batch entry: %w", err)
	}
	return nil
}

func (transaction *Transaction) AssociateOperationWithProjectionBatch(
	ctx context.Context,
	batchID int64,
) error {
	if batchID <= 0 {
		return fmt.Errorf("%w: invalid Projection Batch ID", ErrInvalidOperation)
	}
	if _, err := transaction.ProjectionBatch(ctx, batchID); err != nil {
		return fmt.Errorf("read Projection Batch operation reference: %w", err)
	}
	id := batchID
	transaction.operationScrapeID = &id
	return nil
}

func (transaction *Transaction) RemoveProjectionBatchProjections(
	ctx context.Context,
	batchID int64,
) ([]int64, error) {
	var parentIDs []int64
	var cursorID int64
	for {
		node, err := queryNode(ctx, transaction.transaction, `
			origin_scrape_id = ?
			AND kind = 'PROJECTION_FILE'
			AND id > ?
			ORDER BY id
			LIMIT 1
		`, batchID, cursorID)
		if errors.Is(err, ErrNotFound) {
			return parentIDs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list Projection Batch nodes: %w", err)
		}
		cursorID = node.ID
		if node.ParentID == nil {
			return nil, fmt.Errorf("%w: ProjectionFile has no parent", ErrInvalidNode)
		}

		result, err := transaction.transaction.ExecContext(ctx, `
			DELETE FROM nodes
			WHERE id = ?
			  AND kind = 'PROJECTION_FILE'
			  AND origin_scrape_id = ?
		`, node.ID, batchID)
		if err != nil {
			return nil, classifyMutationError("remove Projection Batch node", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read removed Projection count: %w", err)
		}
		if deleted != 1 {
			return nil, fmt.Errorf(
				"%w: Projection Batch node %d disappeared during transaction",
				ErrInvalidNode,
				node.ID,
			)
		}
		now := transaction.now().UTC().UnixNano()
		if err := transaction.touchDirectory(ctx, *node.ParentID, now); err != nil {
			return nil, err
		}
		transaction.changed = true
		parentIDs = append(parentIDs, *node.ParentID)
	}
}

func (transaction *Transaction) MarkProjectionBatchUndone(
	ctx context.Context,
	batchID int64,
	uid uint32,
	gid uint32,
	undoneAt int64,
) error {
	result, err := transaction.transaction.ExecContext(ctx, `
		UPDATE scrapes
		SET
			status = 'undone',
			undone_by_uid = ?,
			undone_by_gid = ?,
			undone_at = ?
		WHERE id = ?
		  AND status = 'applied'
	`, uid, gid, undoneAt, batchID)
	if err != nil {
		return fmt.Errorf("mark Projection Batch undone: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read undone Projection Batch count: %w", err)
	}
	if updated != 1 {
		return ErrNotFound
	}
	transaction.changed = true
	return nil
}

func (transaction *Transaction) RetainedOverlayPaths(
	ctx context.Context,
	libraryRootID int64,
	targets []string,
) ([]string, error) {
	parentPaths := make([]string, 0, len(targets))
	for _, target := range targets {
		components, err := ValidateRelativePath(target)
		if err != nil {
			return nil, err
		}
		parentPaths = append(parentPaths, strings.Join(components[:len(components)-1], "/"))
	}

	var paths []string
	var cursorID int64
	for {
		node, err := queryNode(ctx, transaction.transaction, `
			kind = 'OVERLAY_FILE'
			AND id > ?
			ORDER BY id
			LIMIT 1
		`, cursorID)
		if errors.Is(err, ErrNotFound) {
			return paths, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list retained OverlayFiles: %w", err)
		}
		cursorID = node.ID

		relativePath, err := nodePathToRoot(
			ctx,
			transaction.transaction,
			node.ID,
			libraryRootID,
		)
		if errors.Is(err, ErrInvalidRoot) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, parentPath := range parentPaths {
			if parentPath == "" ||
				strings.HasPrefix(relativePath, parentPath+"/") {
				paths = append(paths, relativePath)
				break
			}
		}
	}
}

func (transaction *Transaction) validateParent(
	ctx context.Context,
	request CreateNode,
) error {
	parent, err := queryNode(ctx, transaction.transaction, "id = ?", request.ParentID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf(
				"%w: parent node %d does not exist",
				ErrInvalidNode,
				request.ParentID,
			)
		}
		return fmt.Errorf("read node parent: %w", err)
	}

	switch request.Kind {
	case LibraryRoot:
		if parent.Kind != GlobalRoot {
			return fmt.Errorf(
				"%w: LibraryRoot parent is %s, want GlobalRoot",
				ErrInvalidNode,
				parent.Kind,
			)
		}
	case Directory, ProjectionFile, OverlayFile:
		if parent.Kind != LibraryRoot && parent.Kind != Directory {
			return fmt.Errorf(
				"%w: %s parent is %s, want LibraryRoot or Directory",
				ErrInvalidNode,
				request.Kind,
				parent.Kind,
			)
		}
	}
	return nil
}

func (transaction *Transaction) EnsureManagedDirectories(
	ctx context.Context,
	parentID int64,
	components []string,
	attributes Attributes,
	creator Actor,
) (Node, error) {
	if err := validateComponents(components); err != nil {
		return Node{}, err
	}
	if !creator.valid() {
		return Node{}, fmt.Errorf("%w: invalid directory creator", ErrInvalidNode)
	}

	current, err := queryNode(ctx, transaction.transaction, "id = ?", parentID)
	if err != nil {
		return Node{}, fmt.Errorf("read managed-directory parent: %w", err)
	}
	if current.Kind != LibraryRoot && current.Kind != Directory {
		return Node{}, fmt.Errorf(
			"%w: node %d is %s",
			ErrInvalidRoot,
			parentID,
			current.Kind,
		)
	}

	for _, component := range components {
		child, err := queryNode(
			ctx,
			transaction.transaction,
			"parent_id = ? AND name = ?",
			current.ID,
			component,
		)
		if err == nil {
			if child.Kind != Directory {
				return Node{}, fmt.Errorf(
					"%w: %q is %s",
					ErrConflict,
					component,
					child.Kind,
				)
			}
			current = child
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return Node{}, fmt.Errorf("lookup managed directory %q: %w", component, err)
		}

		current, err = transaction.CreateNode(ctx, CreateNode{
			ParentID:         current.ID,
			Name:             component,
			Kind:             Directory,
			Creator:          creator,
			ManagedDirectory: true,
			Attributes:       attributes,
		})
		if err != nil {
			return Node{}, fmt.Errorf("create managed directory %q: %w", component, err)
		}
	}
	return current, nil
}

func (transaction *Transaction) PruneManagedDirectories(
	ctx context.Context,
	libraryRootID int64,
	startNodeID int64,
) error {
	root, err := queryNode(ctx, transaction.transaction, "id = ?", libraryRootID)
	if err != nil {
		return fmt.Errorf("read prune root: %w", err)
	}
	if root.Kind != LibraryRoot {
		return fmt.Errorf("%w: node %d is %s", ErrInvalidRoot, root.ID, root.Kind)
	}

	path, err := transaction.pathToRoot(ctx, startNodeID, libraryRootID)
	if err != nil {
		return err
	}
	for _, nodeID := range path {
		node, err := queryNode(ctx, transaction.transaction, "id = ?", nodeID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return fmt.Errorf("read managed directory %d: %w", nodeID, err)
		}
		if node.Kind != Directory || !node.ManagedDirectory {
			return nil
		}

		result, err := transaction.transaction.ExecContext(ctx, `
			DELETE FROM nodes
			WHERE id = ?
			  AND kind = 'DIRECTORY'
			  AND managed_directory = 1
			  AND NOT EXISTS (
				SELECT 1
				FROM nodes AS child
				WHERE child.parent_id = nodes.id
			  )
		`, node.ID)
		if err != nil {
			return classifyMutationError("prune managed directory", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read pruned directory count: %w", err)
		}
		if deleted == 0 {
			return nil
		}
		if node.ParentID != nil {
			if err := transaction.touchDirectory(
				ctx,
				*node.ParentID,
				transaction.now().UTC().UnixNano(),
			); err != nil {
				return err
			}
		}
		transaction.changed = true
	}
	return nil
}

func (transaction *Transaction) pathToRoot(
	ctx context.Context,
	startNodeID int64,
	rootNodeID int64,
) ([]int64, error) {
	if startNodeID == rootNodeID {
		return nil, nil
	}

	var path []int64
	seen := map[int64]struct{}{}
	currentID := startNodeID
	for {
		if _, exists := seen[currentID]; exists {
			return nil, fmt.Errorf("%w: node ancestry contains a cycle", ErrInvalidRoot)
		}
		seen[currentID] = struct{}{}

		node, err := queryNode(ctx, transaction.transaction, "id = ?", currentID)
		if err != nil {
			return nil, fmt.Errorf("read node ancestry: %w", err)
		}
		path = append(path, node.ID)
		if node.ParentID == nil {
			return nil, fmt.Errorf(
				"%w: node %d is outside LibraryRoot %d",
				ErrInvalidRoot,
				startNodeID,
				rootNodeID,
			)
		}
		if *node.ParentID == rootNodeID {
			return path, nil
		}
		currentID = *node.ParentID
	}
}

func (transaction *Transaction) touchDirectory(
	ctx context.Context,
	nodeID int64,
	now int64,
) error {
	if _, err := transaction.transaction.ExecContext(ctx, `
		UPDATE nodes
		SET mtime_ns = ?, virtual_ctime_ns = ?, updated_at = ?
		WHERE id = ?
		  AND kind IN ('GLOBAL_ROOT', 'LIBRARY_ROOT', 'DIRECTORY')
	`, now, now, now, nodeID); err != nil {
		return fmt.Errorf("update parent directory timestamps: %w", err)
	}
	return nil
}

func validateOperation(operation Operation, change func(*Transaction) error) error {
	if strings.TrimSpace(operation.Name) == "" {
		return fmt.Errorf("%w: operation name must not be empty", ErrInvalidOperation)
	}
	if !operation.Actor.valid() {
		return fmt.Errorf("%w: invalid actor", ErrInvalidOperation)
	}
	if operation.ScrapeID != nil && *operation.ScrapeID <= 0 {
		return fmt.Errorf("%w: invalid Scrape ID", ErrInvalidOperation)
	}
	if len(operation.Details.Paths) == 0 {
		return fmt.Errorf("%w: details paths must not be empty", ErrInvalidOperation)
	}
	if len(operation.Details.Paths) != len(operation.Details.NodeKinds) {
		return fmt.Errorf(
			"%w: details paths and node kinds must have equal lengths",
			ErrInvalidOperation,
		)
	}
	for index, path := range operation.Details.Paths {
		if path == "" {
			return fmt.Errorf(
				"%w: details path %d must not be empty",
				ErrInvalidOperation,
				index,
			)
		}
		if !operation.Details.NodeKinds[index].valid() {
			return fmt.Errorf(
				"%w: invalid details node kind %q",
				ErrInvalidOperation,
				operation.Details.NodeKinds[index],
			)
		}
	}
	if change == nil {
		return fmt.Errorf("%w: change callback must not be nil", ErrInvalidOperation)
	}
	return nil
}

func validateCreateNode(request CreateNode) error {
	if request.ParentID <= 0 {
		return fmt.Errorf("%w: parent ID must be positive", ErrInvalidNode)
	}
	if err := validateName(request.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	if !request.Kind.valid() || request.Kind == GlobalRoot {
		return fmt.Errorf("%w: invalid creatable node kind %q", ErrInvalidNode, request.Kind)
	}
	if !request.Creator.valid() {
		return fmt.Errorf("%w: invalid creator", ErrInvalidNode)
	}
	if request.Attributes.Mode != request.Attributes.Mode.Perm() {
		return fmt.Errorf("%w: mode must contain only permission bits", ErrInvalidNode)
	}

	switch request.Kind {
	case ProjectionFile:
		if request.Source == nil || request.ObjectID != nil || request.ManagedDirectory {
			return fmt.Errorf("%w: invalid ProjectionFile fields", ErrInvalidNode)
		}
		if request.Attributes.Mode.Perm()&0o222 != 0 {
			return fmt.Errorf("%w: ProjectionFile mode must be read-only", ErrInvalidNode)
		}
		if !strings.HasPrefix(request.Source.Path, "/") ||
			request.Source.DeviceID < 0 ||
			request.Source.Inode < 0 ||
			request.Source.Size < 0 {
			return fmt.Errorf("%w: invalid ProjectionFile source", ErrInvalidNode)
		}
	case OverlayFile:
		if request.Source != nil || request.ObjectID == nil || request.ManagedDirectory {
			return fmt.Errorf("%w: invalid OverlayFile fields", ErrInvalidNode)
		}
	case LibraryRoot:
		if request.Source != nil ||
			request.ObjectID != nil ||
			request.OriginScrapeID != nil ||
			request.ManagedDirectory {
			return fmt.Errorf("%w: invalid LibraryRoot fields", ErrInvalidNode)
		}
	case Directory:
		if request.Source != nil || request.ObjectID != nil {
			return fmt.Errorf("%w: invalid Directory fields", ErrInvalidNode)
		}
	}
	return nil
}

func validateCreateProjectionBatch(request CreateProjectionBatch) error {
	if request.LibraryID <= 0 {
		return fmt.Errorf("%w: invalid Projection Batch Library ID", ErrInvalidOperation)
	}
	if strings.TrimSpace(request.ProfileName) == "" || request.ProfileVersion <= 0 {
		return fmt.Errorf("%w: invalid Projection Batch Profile", ErrInvalidOperation)
	}
	if !validDigest(request.Digest) {
		return fmt.Errorf("%w: invalid Projection Batch digest", ErrInvalidOperation)
	}
	if request.AppliedAt <= 0 {
		return fmt.Errorf("%w: invalid Projection Batch apply time", ErrInvalidOperation)
	}
	return nil
}

func validateCreateProjectionBatchEntry(request CreateProjectionBatchEntry) error {
	if request.BatchID <= 0 ||
		request.Position < 0 ||
		request.ProjectionNodeID <= 0 {
		return fmt.Errorf("%w: invalid Projection Batch entry identity", ErrInvalidNode)
	}
	return validateProjectionBatchEntryData(
		request.BatchID,
		request.Position,
		request.Source,
		request.Target,
	)
}

func validateProjectionBatchEntryData(
	batchID int64,
	position int64,
	source ProjectionSource,
	target string,
) error {
	if batchID <= 0 || position < 0 {
		return fmt.Errorf("%w: invalid Projection Batch entry identity", ErrInvalidNode)
	}
	if _, err := ValidateRelativePath(target); err != nil {
		return fmt.Errorf("%w: invalid Projection Batch target: %v", ErrInvalidNode, err)
	}
	if !strings.HasPrefix(source.Path, "/") ||
		source.DeviceID < 0 ||
		source.Inode < 0 ||
		source.Size < 0 {
		return fmt.Errorf("%w: invalid Projection Batch source", ErrInvalidNode)
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateCreateLibrary(request CreateLibrary) error {
	if err := validateName(request.Name); err != nil {
		return fmt.Errorf("%w: invalid name: %v", ErrInvalidLibrary, err)
	}
	if strings.TrimSpace(request.ProfileName) == "" {
		return fmt.Errorf("%w: Profile name must not be empty", ErrInvalidLibrary)
	}
	if request.ProfileVersion <= 0 {
		return fmt.Errorf("%w: Profile version must be positive", ErrInvalidLibrary)
	}
	if !request.Creator.valid() {
		return fmt.Errorf("%w: invalid creator", ErrInvalidLibrary)
	}

	modes := []struct {
		name string
		mode fs.FileMode
	}{
		{name: "root", mode: request.RootMode},
		{name: "directory", mode: request.DirectoryMode},
		{name: "projection", mode: request.ProjectionMode},
		{name: "overlay", mode: request.OverlayMode},
	}
	for _, mode := range modes {
		if mode.mode != mode.mode.Perm() {
			return fmt.Errorf(
				"%w: %s mode must contain only permission bits",
				ErrInvalidLibrary,
				mode.name,
			)
		}
	}
	if request.ProjectionMode.Perm()&0o222 != 0 {
		return fmt.Errorf(
			"%w: projection mode must be read-only",
			ErrInvalidLibrary,
		)
	}
	switch {
	case request.ProfileName == "jellyfin" && request.ProfileVersion == 1,
		request.ProfileName == "plex" && request.ProfileVersion == 1:
	default:
		return fmt.Errorf(
			"%w: %s@%d",
			ErrProfileNotFound,
			request.ProfileName,
			request.ProfileVersion,
		)
	}
	return nil
}

func classifyMutationError(action string, err error) error {
	var sqliteError *sqlite.Error
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() {
		case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
			return fmt.Errorf("%s: %w: %v", action, ErrConflict, err)
		case sqlite3.SQLITE_CONSTRAINT_CHECK,
			sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY,
			sqlite3.SQLITE_CONSTRAINT_NOTNULL:
			return fmt.Errorf("%s: %w: %v", action, ErrInvalidNode, err)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

func classifyLibraryMutationError(action string, err error) error {
	var sqliteError *sqlite.Error
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() {
		case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
			return fmt.Errorf("%s: %w: %v", action, ErrConflict, err)
		case sqlite3.SQLITE_CONSTRAINT_CHECK,
			sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY,
			sqlite3.SQLITE_CONSTRAINT_NOTNULL:
			return fmt.Errorf("%s: %w: %v", action, ErrInvalidLibrary, err)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

type nodeQuery interface {
	QueryRowContext(context.Context, string, ...any) *database.Row
}

func globalRoot(ctx context.Context, query nodeQuery) (Node, error) {
	node, err := queryNode(ctx, query, "id = ?", int64(1))
	if err != nil {
		return Node{}, fmt.Errorf("read global root: %w", err)
	}
	if node.Kind != GlobalRoot {
		return Node{}, fmt.Errorf("%w: node 1 is %s", ErrInvalidRoot, node.Kind)
	}
	return node, nil
}

func namespaceVersion(ctx context.Context, query nodeQuery) (int64, error) {
	var version int64
	if err := query.QueryRowContext(
		ctx,
		"SELECT version FROM namespace_state WHERE singleton = 1",
	).Scan(&version); err != nil {
		return 0, fmt.Errorf("read namespace version: %w", err)
	}
	return version, nil
}

func queryProjectionBatch(
	ctx context.Context,
	query nodeQuery,
	predicate string,
	arguments ...any,
) (ProjectionBatch, error) {
	row := query.QueryRowContext(ctx, `
		SELECT
			scrapes.id,
			scrapes.library_id,
			libraries.name,
			libraries.root_node_id,
			scrapes.profile_name,
			scrapes.profile_version,
			scrapes.digest,
			scrapes.status,
			scrapes.applied_by_uid,
			scrapes.applied_by_gid,
			scrapes.applied_at,
			scrapes.undone_by_uid,
			scrapes.undone_by_gid,
			scrapes.undone_at
		FROM scrapes
		JOIN libraries ON libraries.id = scrapes.library_id
		WHERE `+predicate,
		arguments...,
	)

	var batch ProjectionBatch
	var appliedByUID int64
	var appliedByGID int64
	var libraryRootID sql.NullInt64
	var undoneByUID sql.NullInt64
	var undoneByGID sql.NullInt64
	var undoneAt sql.NullInt64
	if err := row.Scan(
		&batch.ID,
		&batch.LibraryID,
		&batch.LibraryName,
		&libraryRootID,
		&batch.ProfileName,
		&batch.ProfileVersion,
		&batch.Digest,
		&batch.Status,
		&appliedByUID,
		&appliedByGID,
		&batch.AppliedAt,
		&undoneByUID,
		&undoneByGID,
		&undoneAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectionBatch{}, ErrNotFound
		}
		return ProjectionBatch{}, err
	}
	batch.AppliedByUID = uint32(appliedByUID)
	batch.AppliedByGID = uint32(appliedByGID)
	if libraryRootID.Valid {
		batch.LibraryRootID = libraryRootID.Int64
		batch.LibraryRootAvailable = true
	}
	batch.UndoneByUID = nullableUint32(undoneByUID)
	batch.UndoneByGID = nullableUint32(undoneByGID)
	batch.UndoneAt = nullableInt64(undoneAt)
	if err := validatePersistedProjectionBatch(batch); err != nil {
		return ProjectionBatch{}, err
	}
	return batch, nil
}

func projectionBatchEntries(
	ctx context.Context,
	query nodeQuery,
	batchID int64,
) ([]ProjectionBatchEntry, error) {
	var entries []ProjectionBatchEntry
	var cursorPosition int64 = -1
	for {
		entry, err := queryProjectionBatchEntry(
			ctx,
			query,
			batchID,
			cursorPosition,
		)
		if errors.Is(err, ErrNotFound) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list Projection Batch entries: %w", err)
		}
		entries = append(entries, entry)
		cursorPosition = entry.Position
	}
}

func queryProjectionBatchEntry(
	ctx context.Context,
	query nodeQuery,
	batchID int64,
	afterPosition int64,
) (ProjectionBatchEntry, error) {
	row := query.QueryRowContext(ctx, `
		SELECT
			position,
			source_path,
			source_device_id,
			source_inode,
			cached_size,
			cached_atime_ns,
			cached_mtime_ns,
			cached_source_ctime_ns,
			verified_at_ns,
			target,
			projection_node_id
		FROM projection_batch_entries
		WHERE scrape_id = ?
		  AND position > ?
		ORDER BY position
		LIMIT 1
	`, batchID, afterPosition)

	var entry ProjectionBatchEntry
	var projectionNodeID sql.NullInt64
	if err := row.Scan(
		&entry.Position,
		&entry.Source.Path,
		&entry.Source.DeviceID,
		&entry.Source.Inode,
		&entry.Source.Size,
		&entry.Source.AtimeNS,
		&entry.Source.MtimeNS,
		&entry.Source.SourceCtimeNS,
		&entry.Source.VerifiedAtNS,
		&entry.Target,
		&projectionNodeID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectionBatchEntry{}, ErrNotFound
		}
		return ProjectionBatchEntry{}, err
	}
	entry.ProjectionNodeID = nullableInt64(projectionNodeID)
	if entry.ProjectionNodeID != nil && *entry.ProjectionNodeID <= 0 {
		return ProjectionBatchEntry{}, fmt.Errorf(
			"%w: invalid persisted Projection node ID",
			ErrInvalidNode,
		)
	}
	if err := validateProjectionBatchEntryData(
		batchID,
		entry.Position,
		entry.Source,
		entry.Target,
	); err != nil {
		return ProjectionBatchEntry{}, fmt.Errorf(
			"%w: persisted Projection Batch entry: %v",
			ErrInvalidNode,
			err,
		)
	}
	return entry, nil
}

func queryProjectionBatchOperation(
	ctx context.Context,
	query nodeQuery,
	batchID int64,
	afterID int64,
) (ProjectionBatchOperation, error) {
	row := query.QueryRowContext(ctx, `
		SELECT
			id,
			operation,
			actor_kind,
			actor_id,
			namespace_version,
			details_json,
			created_at
		FROM operation_log
		WHERE scrape_id = ?
		  AND id > ?
		ORDER BY id
		LIMIT 1
	`, batchID, afterID)

	var operation ProjectionBatchOperation
	var actorID sql.NullInt64
	var detailsJSON string
	if err := row.Scan(
		&operation.ID,
		&operation.Name,
		&operation.Actor.Kind,
		&actorID,
		&operation.NamespaceVersion,
		&detailsJSON,
		&operation.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectionBatchOperation{}, ErrNotFound
		}
		return ProjectionBatchOperation{}, err
	}
	operation.Actor.ID = nullableInt64(actorID)
	if err := json.Unmarshal([]byte(detailsJSON), &operation.Details); err != nil {
		return ProjectionBatchOperation{}, fmt.Errorf(
			"%w: decode Projection Batch operation details: %v",
			ErrInvalidOperation,
			err,
		)
	}
	batchReference := batchID
	if err := validateOperation(Operation{
		Name:     operation.Name,
		Actor:    operation.Actor,
		ScrapeID: &batchReference,
		Details:  operation.Details,
	}, func(*Transaction) error { return nil }); err != nil {
		return ProjectionBatchOperation{}, err
	}
	return operation, nil
}

func projectionBatchOperations(
	ctx context.Context,
	query nodeQuery,
	batchID int64,
) ([]ProjectionBatchOperation, error) {
	var operations []ProjectionBatchOperation
	var cursorID int64
	for {
		operation, err := queryProjectionBatchOperation(
			ctx,
			query,
			batchID,
			cursorID,
		)
		if errors.Is(err, ErrNotFound) {
			return operations, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list Projection Batch operations: %w", err)
		}
		operations = append(operations, operation)
		cursorID = operation.ID
	}
}

func validatePersistedProjectionBatch(batch ProjectionBatch) error {
	if batch.ID <= 0 ||
		batch.LibraryID <= 0 ||
		batch.LibraryName == "" ||
		batch.ProfileName == "" ||
		batch.ProfileVersion <= 0 ||
		!validDigest(batch.Digest) ||
		batch.AppliedAt <= 0 {
		return fmt.Errorf(
			"%w: Projection Batch %d has invalid provenance",
			ErrInvalidOperation,
			batch.ID,
		)
	}
	if batch.LibraryRootAvailable {
		if batch.LibraryRootID <= 0 {
			return fmt.Errorf(
				"%w: Projection Batch %d has an invalid LibraryRoot",
				ErrInvalidOperation,
				batch.ID,
			)
		}
	} else if batch.LibraryRootID != 0 {
		return fmt.Errorf(
			"%w: Projection Batch %d has unavailable LibraryRoot state",
			ErrInvalidOperation,
			batch.ID,
		)
	}
	switch batch.Status {
	case ProjectionBatchApplied:
		if batch.UndoneByUID != nil || batch.UndoneByGID != nil || batch.UndoneAt != nil {
			return fmt.Errorf(
				"%w: applied Projection Batch %d has undo provenance",
				ErrInvalidOperation,
				batch.ID,
			)
		}
	case ProjectionBatchUndone:
		if batch.UndoneByUID == nil || batch.UndoneByGID == nil || batch.UndoneAt == nil {
			return fmt.Errorf(
				"%w: undone Projection Batch %d lacks undo provenance",
				ErrInvalidOperation,
				batch.ID,
			)
		}
	default:
		return fmt.Errorf(
			"%w: Projection Batch %d has status %q",
			ErrInvalidOperation,
			batch.ID,
			batch.Status,
		)
	}
	return nil
}

func queryLibrary(
	ctx context.Context,
	query nodeQuery,
	predicate string,
	arguments ...any,
) (Library, error) {
	row := query.QueryRowContext(ctx, `
		SELECT
			libraries.id,
			libraries.name,
			libraries.status,
			libraries.root_node_id,
			libraries.profile_name,
			libraries.profile_version,
			libraries.uid,
			libraries.gid,
			libraries.root_mode,
			libraries.directory_mode,
			libraries.projection_file_mode,
			libraries.overlay_file_mode,
			libraries.created_at,
			libraries.updated_at,
			root.id,
			root.parent_id,
			root.name,
			root.kind,
			root.mode,
			root.uid,
			root.gid
		FROM libraries
		LEFT JOIN nodes AS root ON root.id = libraries.root_node_id
		WHERE `+predicate,
		arguments...,
	)

	var library Library
	var uid int64
	var gid int64
	var rootMode int64
	var directoryMode int64
	var projectionMode int64
	var overlayMode int64
	var joinedRootID sql.NullInt64
	var rootParentID sql.NullInt64
	var rootName sql.NullString
	var rootKind sql.NullString
	var joinedRootMode sql.NullInt64
	var rootUID sql.NullInt64
	var rootGID sql.NullInt64
	if err := row.Scan(
		&library.ID,
		&library.Name,
		&library.Status,
		&library.RootNodeID,
		&library.ProfileName,
		&library.ProfileVersion,
		&uid,
		&gid,
		&rootMode,
		&directoryMode,
		&projectionMode,
		&overlayMode,
		&library.CreatedAt,
		&library.UpdatedAt,
		&joinedRootID,
		&rootParentID,
		&rootName,
		&rootKind,
		&joinedRootMode,
		&rootUID,
		&rootGID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Library{}, ErrNotFound
		}
		return Library{}, err
	}

	library.UID = uint32(uid)
	library.GID = uint32(gid)
	library.RootMode = fs.FileMode(rootMode)
	library.DirectoryMode = fs.FileMode(directoryMode)
	library.ProjectionMode = fs.FileMode(projectionMode)
	library.OverlayMode = fs.FileMode(overlayMode)
	if err := validatePersistedLibrary(
		library,
		joinedRootID,
		rootParentID,
		rootName,
		rootKind,
		joinedRootMode,
		rootUID,
		rootGID,
	); err != nil {
		return Library{}, err
	}
	return library, nil
}

func validatePersistedLibrary(
	library Library,
	joinedRootID sql.NullInt64,
	rootParentID sql.NullInt64,
	rootName sql.NullString,
	rootKind sql.NullString,
	joinedRootMode sql.NullInt64,
	rootUID sql.NullInt64,
	rootGID sql.NullInt64,
) error {
	if library.Status != LibraryActive {
		return fmt.Errorf(
			"%w: Library %d has status %q",
			ErrInvalidLibrary,
			library.ID,
			library.Status,
		)
	}
	if err := (CreateLibrary{
		Name:           library.Name,
		ProfileName:    library.ProfileName,
		ProfileVersion: library.ProfileVersion,
		Creator:        Actor{Kind: ActorSystem},
		UID:            library.UID,
		GID:            library.GID,
		RootMode:       library.RootMode,
		DirectoryMode:  library.DirectoryMode,
		ProjectionMode: library.ProjectionMode,
		OverlayMode:    library.OverlayMode,
	}).Validate(); err != nil {
		return fmt.Errorf("%w: Library %d configuration: %v", ErrInvalidLibrary, library.ID, err)
	}
	if !joinedRootID.Valid ||
		joinedRootID.Int64 != library.RootNodeID ||
		!rootParentID.Valid ||
		rootParentID.Int64 != 1 ||
		!rootName.Valid ||
		rootName.String != library.Name ||
		!rootKind.Valid ||
		NodeKind(rootKind.String) != LibraryRoot ||
		!joinedRootMode.Valid ||
		fs.FileMode(joinedRootMode.Int64) != library.RootMode ||
		!rootUID.Valid ||
		uint32(rootUID.Int64) != library.UID ||
		!rootGID.Valid ||
		uint32(rootGID.Int64) != library.GID {
		return fmt.Errorf(
			"%w: Library %d has a mismatched root node",
			ErrInvalidLibrary,
			library.ID,
		)
	}
	return nil
}

func lookupComponents(
	ctx context.Context,
	query nodeQuery,
	libraryRootID int64,
	components []string,
) (Node, error) {
	current, err := queryNode(ctx, query, "id = ?", libraryRootID)
	if err != nil {
		return Node{}, fmt.Errorf("read library root: %w", err)
	}
	if current.Kind != LibraryRoot {
		return Node{}, fmt.Errorf(
			"%w: node %d is %s",
			ErrInvalidRoot,
			libraryRootID,
			current.Kind,
		)
	}

	for _, component := range components {
		current, err = queryNode(
			ctx,
			query,
			"parent_id = ? AND name = ?",
			current.ID,
			component,
		)
		if err != nil {
			return Node{}, fmt.Errorf("lookup component %q: %w", component, err)
		}
	}
	return current, nil
}

func checkTargetAvailable(
	ctx context.Context,
	query nodeQuery,
	libraryRootID int64,
	components []string,
) error {
	current, err := queryNode(ctx, query, "id = ?", libraryRootID)
	if err != nil {
		return fmt.Errorf("read library root: %w", err)
	}
	if current.Kind != LibraryRoot {
		return fmt.Errorf(
			"%w: node %d is %s",
			ErrInvalidRoot,
			libraryRootID,
			current.Kind,
		)
	}

	for index, component := range components {
		child, err := queryNode(
			ctx,
			query,
			"parent_id = ? AND name = ?",
			current.ID,
			component,
		)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("check target component %q: %w", component, err)
		}
		if index == len(components)-1 {
			return fmt.Errorf("%w: target already exists", ErrConflict)
		}
		if child.Kind != Directory {
			return fmt.Errorf(
				"%w: target parent %q is %s",
				ErrConflict,
				component,
				child.Kind,
			)
		}
		current = child
	}
	return nil
}

func nodePathToRoot(
	ctx context.Context,
	query nodeQuery,
	nodeID int64,
	rootNodeID int64,
) (string, error) {
	var components []string
	seen := map[int64]struct{}{}
	currentID := nodeID
	for currentID != rootNodeID {
		if _, duplicate := seen[currentID]; duplicate {
			return "", fmt.Errorf("%w: node ancestry contains a cycle", ErrInvalidRoot)
		}
		seen[currentID] = struct{}{}

		node, err := queryNode(ctx, query, "id = ?", currentID)
		if err != nil {
			return "", fmt.Errorf("read node path: %w", err)
		}
		components = append(components, node.Name)
		if node.ParentID == nil {
			return "", fmt.Errorf(
				"%w: node %d is outside LibraryRoot %d",
				ErrInvalidRoot,
				nodeID,
				rootNodeID,
			)
		}
		currentID = *node.ParentID
	}

	for left, right := 0, len(components)-1; left < right; left, right = left+1, right-1 {
		components[left], components[right] = components[right], components[left]
	}
	return strings.Join(components, "/"), nil
}

func queryNode(
	ctx context.Context,
	query nodeQuery,
	predicate string,
	arguments ...any,
) (Node, error) {
	row := query.QueryRowContext(ctx, `
		SELECT
			id,
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
		FROM nodes
		WHERE `+predicate,
		arguments...,
	)

	var node Node
	var parentID sql.NullInt64
	var sourcePath sql.NullString
	var sourceDeviceID sql.NullInt64
	var sourceInode sql.NullInt64
	var cachedSize sql.NullInt64
	var cachedAtimeNS sql.NullInt64
	var cachedMtimeNS sql.NullInt64
	var cachedSourceCtimeNS sql.NullInt64
	var verifiedAtNS sql.NullInt64
	var objectID sql.NullInt64
	var originScrapeID sql.NullInt64
	var creatorID sql.NullInt64
	var managedDirectory int64
	var mode int64
	var uid int64
	var gid int64
	if err := row.Scan(
		&node.ID,
		&parentID,
		&node.Name,
		&node.Kind,
		&sourcePath,
		&sourceDeviceID,
		&sourceInode,
		&cachedSize,
		&cachedAtimeNS,
		&cachedMtimeNS,
		&cachedSourceCtimeNS,
		&verifiedAtNS,
		&objectID,
		&originScrapeID,
		&node.Creator.Kind,
		&creatorID,
		&managedDirectory,
		&mode,
		&uid,
		&gid,
		&node.AtimeNS,
		&node.MtimeNS,
		&node.VirtualCtimeNS,
		&node.CreatedAt,
		&node.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}

	node.ParentID = nullableInt64(parentID)
	node.ObjectID = nullableInt64(objectID)
	node.OriginScrapeID = nullableInt64(originScrapeID)
	node.Creator.ID = nullableInt64(creatorID)
	node.ManagedDirectory = managedDirectory == 1
	node.Attributes = Attributes{
		Mode: fs.FileMode(mode),
		UID:  uint32(uid),
		GID:  uint32(gid),
	}
	if sourcePath.Valid {
		node.Source = &ProjectionSource{
			Path:          sourcePath.String,
			DeviceID:      sourceDeviceID.Int64,
			Inode:         sourceInode.Int64,
			Size:          cachedSize.Int64,
			AtimeNS:       cachedAtimeNS.Int64,
			MtimeNS:       cachedMtimeNS.Int64,
			SourceCtimeNS: cachedSourceCtimeNS.Int64,
			VerifiedAtNS:  verifiedAtNS.Int64,
		}
	}
	return node, nil
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullableUint32(value sql.NullInt64) *uint32 {
	if !value.Valid {
		return nil
	}
	result := uint32(value.Int64)
	return &result
}
