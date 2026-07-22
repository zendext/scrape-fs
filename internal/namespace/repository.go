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
	err := repository.useConnection(ctx, func(connection *sql.Conn) error {
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
	if err := repository.useConnection(ctx, func(connection *sql.Conn) error {
		var err error
		version, err = namespaceVersion(ctx, connection)
		return err
	}); err != nil {
		return 0, err
	}
	return version, nil
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
	err = repository.useConnection(ctx, func(connection *sql.Conn) error {
		var err error
		current, err = lookupComponents(ctx, connection, libraryRootID, components)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return current, nil
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
	err = repository.useConnection(ctx, func(connection *sql.Conn) error {
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
	use func(*sql.Conn) error,
) error {
	return repository.database.UseConnection(ctx, use)
}

func (repository *Repository) update(
	ctx context.Context,
	connection *sql.Conn,
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
		operation.ScrapeID,
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
	transaction *sql.Tx
	now         func() time.Time
	changed     bool
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

func (transaction *Transaction) CreateNode(
	ctx context.Context,
	request CreateNode,
) (Node, error) {
	if err := validateCreateNode(request); err != nil {
		return Node{}, err
	}
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

type nodeQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
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
