package namespace

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	databasepkg "github.com/zendext/scrape-fs/internal/database"
)

func TestRepositoryUpdateCommitsVersionAndLogAtomically(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()

	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	var libraryRootID int64
	version, err := repository.Update(ctx, testOperation("library.create"), func(transaction *Transaction) error {
		node, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: root.ID,
			Name:     "library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		})
		libraryRootID = node.ID
		return err
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}

	node, err := repository.Lookup(ctx, libraryRootID, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup missing error = %v, want ErrNotFound; node=%#v", err, node)
	}
	assertVersionAndLogCount(t, database, 1, 1)

	var operation string
	var actorKind string
	var actorID *int64
	var loggedVersion int64
	var details string
	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(ctx, `
			SELECT operation, actor_kind, actor_id, namespace_version, details_json
			FROM operation_log
		`).Scan(&operation, &actorKind, &actorID, &loggedVersion, &details)
	}); err != nil {
		t.Fatalf("read operation log: %v", err)
	}
	if operation != "library.create" {
		t.Errorf("operation = %q, want library.create", operation)
	}
	if actorKind != "system" || actorID != nil {
		t.Errorf("actor = %s/%v, want system/NULL", actorKind, actorID)
	}
	if loggedVersion != 1 {
		t.Errorf("logged version = %d, want 1", loggedVersion)
	}
	if !json.Valid([]byte(details)) {
		t.Errorf("details = %q, want valid JSON", details)
	}
}

func TestRepositoryUpdateRollsBackNodesVersionAndLog(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	transactionFailure := errors.New("transaction failed")
	_, err = repository.Update(ctx, testOperation("library.create"), func(transaction *Transaction) error {
		if _, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: root.ID,
			Name:     "rolled-back",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		}); err != nil {
			return err
		}
		return transactionFailure
	})
	if !errors.Is(err, transactionFailure) {
		t.Fatalf("Update error = %v, want transaction failure", err)
	}

	assertVersionAndLogCount(t, database, 0, 0)
	var count int
	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM nodes WHERE name = 'rolled-back'",
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count rolled-back node: %v", err)
	}
	if count != 0 {
		t.Errorf("rolled-back node count = %d, want 0", count)
	}
}

func TestRepositoryPersistsProjectionBatchHistoryAndOperationReference(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	repository.now = func() time.Time {
		return time.Unix(1_700_000_000, 123)
	}
	ctx := context.Background()

	var library Library
	if _, err := repository.Update(
		ctx,
		testOperation("library.create"),
		func(transaction *Transaction) error {
			var err error
			library, err = transaction.CreateLibrary(ctx, CreateLibrary{
				Name:           "media",
				ProfileName:    "jellyfin",
				ProfileVersion: 1,
				Creator:        Actor{Kind: ActorSystem},
				UID:            1000,
				GID:            1001,
				RootMode:       0o775,
				DirectoryMode:  0o775,
				ProjectionMode: 0o444,
				OverlayMode:    0o664,
			})
			return err
		},
	); err != nil {
		t.Fatalf("Create Library: %v", err)
	}

	source := ProjectionSource{
		Path:          "/srv/media/Dune.mkv",
		DeviceID:      1,
		Inode:         2,
		Size:          3,
		AtimeNS:       4,
		MtimeNS:       5,
		SourceCtimeNS: 6,
		VerifiedAtNS:  7,
	}
	var batch ProjectionBatch
	version, err := repository.Update(ctx, Operation{
		Name:  "projection.batch.apply",
		Actor: Actor{Kind: ActorSystem},
		Details: OperationDetails{
			Paths:     []string{"media:/Movies/Dune.mkv"},
			NodeKinds: []NodeKind{ProjectionFile},
		},
	}, func(transaction *Transaction) error {
		var err error
		batch, err = transaction.CreateProjectionBatch(ctx, CreateProjectionBatch{
			LibraryID:      library.ID,
			ProfileName:    library.ProfileName,
			ProfileVersion: library.ProfileVersion,
			Digest:         "sha256:" + strings.Repeat("a", 64),
			AppliedByUID:   1000,
			AppliedByGID:   1001,
			AppliedAt:      repository.now().UTC().UnixNano(),
		})
		if err != nil {
			return err
		}
		if err := transaction.AssociateOperationWithProjectionBatch(ctx, batch.ID); err != nil {
			return err
		}
		parent, err := transaction.EnsureManagedDirectories(
			ctx,
			library.RootNodeID,
			[]string{"Movies"},
			Attributes{
				Mode: library.DirectoryMode,
				UID:  library.UID,
				GID:  library.GID,
			},
			Actor{Kind: ActorSystem},
		)
		if err != nil {
			return err
		}
		node, err := transaction.CreateNode(ctx, CreateNode{
			ParentID:       parent.ID,
			Name:           "Dune.mkv",
			Kind:           ProjectionFile,
			Source:         &source,
			OriginScrapeID: &batch.ID,
			Creator:        Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: library.ProjectionMode,
				UID:  library.UID,
				GID:  library.GID,
			},
		})
		if err != nil {
			return err
		}
		return transaction.CreateProjectionBatchEntry(ctx, CreateProjectionBatchEntry{
			BatchID:          batch.ID,
			Position:         0,
			Source:           source,
			Target:           "Movies/Dune.mkv",
			ProjectionNodeID: node.ID,
		})
	})
	if err != nil {
		t.Fatalf("apply Projection Batch: %v", err)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}

	batches, err := repository.ProjectionBatches(ctx)
	if err != nil {
		t.Fatalf("ProjectionBatches: %v", err)
	}
	if len(batches) != 1 || batches[0] != batch {
		t.Errorf("batches = %#v, want %#v", batches, batch)
	}
	entries, err := repository.ProjectionBatchEntries(ctx, batch.ID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries: %v", err)
	}
	if len(entries) != 1 ||
		entries[0].Position != 0 ||
		entries[0].Source != source ||
		entries[0].Target != "Movies/Dune.mkv" ||
		entries[0].ProjectionNodeID == nil {
		t.Errorf("entries = %#v", entries)
	}
	operations, err := repository.ProjectionBatchOperations(ctx, batch.ID)
	if err != nil {
		t.Fatalf("ProjectionBatchOperations: %v", err)
	}
	if len(operations) != 1 ||
		operations[0].Name != "projection.batch.apply" ||
		operations[0].NamespaceVersion != 2 {
		t.Errorf("operations = %#v", operations)
	}
}

func TestTransactionCreateLibraryPersistsMatchingRoot(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()

	var created Library
	version, err := repository.Update(
		ctx,
		testOperation("library.create"),
		func(transaction *Transaction) error {
			var err error
			created, err = transaction.CreateLibrary(ctx, CreateLibrary{
				Name:           "jellyfin",
				ProfileName:    "jellyfin",
				ProfileVersion: 1,
				Creator:        Actor{Kind: ActorSystem},
				UID:            1000,
				GID:            1001,
				RootMode:       0o775,
				DirectoryMode:  0o750,
				ProjectionMode: 0o440,
				OverlayMode:    0o660,
			})
			return err
		},
	)
	if err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if created.ID <= 0 || created.RootNodeID <= 0 {
		t.Fatalf("created Library = %#v, want persisted IDs", created)
	}
	if created.Status != LibraryActive {
		t.Errorf("status = %q, want %q", created.Status, LibraryActive)
	}

	libraries, err := repository.Libraries(ctx)
	if err != nil {
		t.Fatalf("Libraries: %v", err)
	}
	if len(libraries) != 1 {
		t.Fatalf("Libraries count = %d, want 1", len(libraries))
	}
	if libraries[0] != created {
		t.Errorf("listed Library = %#v, want %#v", libraries[0], created)
	}

	var rootName string
	var rootKind NodeKind
	var rootParentID int64
	var rootMode int64
	var rootUID int64
	var rootGID int64
	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(ctx, `
			SELECT name, kind, parent_id, mode, uid, gid
			FROM nodes
			WHERE id = ?
		`, created.RootNodeID).Scan(
			&rootName,
			&rootKind,
			&rootParentID,
			&rootMode,
			&rootUID,
			&rootGID,
		)
	}); err != nil {
		t.Fatalf("read LibraryRoot: %v", err)
	}
	if rootName != created.Name ||
		rootKind != LibraryRoot ||
		rootParentID != 1 ||
		rootMode != int64(created.RootMode) ||
		rootUID != int64(created.UID) ||
		rootGID != int64(created.GID) {
		t.Errorf(
			"LibraryRoot = name %q kind %q parent %d mode %#o uid %d gid %d, want matching Library",
			rootName,
			rootKind,
			rootParentID,
			rootMode,
			rootUID,
			rootGID,
		)
	}
	assertVersionAndLogCount(t, database, 1, 1)
}

func TestRepositoryLibraryReturnsActiveLibraryByExactName(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()

	var created Library
	if _, err := repository.Update(
		ctx,
		testOperation("library.create"),
		func(transaction *Transaction) error {
			var err error
			created, err = transaction.CreateLibrary(ctx, CreateLibrary{
				Name:           "library",
				ProfileName:    "jellyfin",
				ProfileVersion: 1,
				Creator:        Actor{Kind: ActorSystem},
				UID:            1000,
				GID:            1000,
				RootMode:       0o775,
				DirectoryMode:  0o775,
				ProjectionMode: 0o444,
				OverlayMode:    0o664,
			})
			return err
		},
	); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	found, err := repository.Library(ctx, "library")
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if found != created {
		t.Errorf("Library = %#v, want %#v", found, created)
	}

	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, `
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
			) VALUES ('retired', 'RETIRED', NULL, 'jellyfin', 1, 1000, 1000, 509, 509, 292, 436, 1, 1, 1)
		`); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("insert retired Library: %v", err)
	}

	for _, name := range []string{"Library", "missing", "retired"} {
		if _, err := repository.Library(ctx, name); !errors.Is(err, ErrLibraryNotFound) {
			t.Errorf("Library(%q) error = %v, want ErrLibraryNotFound", name, err)
		}
	}
}

func TestTransactionCreateLibraryRollsBackLibraryAndRoot(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	transactionFailure := errors.New("transaction failed")

	_, err := repository.Update(
		ctx,
		testOperation("library.create"),
		func(transaction *Transaction) error {
			if _, err := transaction.CreateLibrary(ctx, CreateLibrary{
				Name:           "rolled-back-library",
				ProfileName:    "jellyfin",
				ProfileVersion: 1,
				Creator:        Actor{Kind: ActorSystem},
				UID:            1000,
				GID:            1000,
				RootMode:       0o775,
				DirectoryMode:  0o775,
				ProjectionMode: 0o444,
				OverlayMode:    0o664,
			}); err != nil {
				return err
			}
			return transactionFailure
		},
	)
	if !errors.Is(err, transactionFailure) {
		t.Fatalf("Update error = %v, want transaction failure", err)
	}

	libraries, listErr := repository.Libraries(ctx)
	if listErr != nil {
		t.Fatalf("Libraries: %v", listErr)
	}
	if len(libraries) != 0 {
		t.Errorf("Libraries = %#v, want empty", libraries)
	}
	var roots int
	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM nodes WHERE kind = 'LIBRARY_ROOT'",
		).Scan(&roots)
	}); err != nil {
		t.Fatalf("count LibraryRoot nodes: %v", err)
	}
	if roots != 0 {
		t.Errorf("LibraryRoot count = %d, want 0", roots)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestTransactionCreateLibraryRejectsUnavailableProfileWithoutMutation(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()

	_, err := repository.Update(
		ctx,
		testOperation("library.create"),
		func(transaction *Transaction) error {
			_, err := transaction.CreateLibrary(ctx, CreateLibrary{
				Name:           "unavailable-profile",
				ProfileName:    "unavailable",
				ProfileVersion: 99,
				Creator:        Actor{Kind: ActorSystem},
				UID:            1000,
				GID:            1000,
				RootMode:       0o775,
				DirectoryMode:  0o775,
				ProjectionMode: 0o444,
				OverlayMode:    0o664,
			})
			return err
		},
	)
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("CreateLibrary error = %v, want ErrProfileNotFound", err)
	}

	libraries, listErr := repository.Libraries(ctx)
	if listErr != nil {
		t.Fatalf("Libraries: %v", listErr)
	}
	if len(libraries) != 0 {
		t.Errorf("Libraries = %#v, want empty", libraries)
	}
	var roots int
	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM nodes WHERE kind = 'LIBRARY_ROOT'",
		).Scan(&roots)
	}); err != nil {
		t.Fatalf("count LibraryRoot nodes: %v", err)
	}
	if roots != 0 {
		t.Errorf("LibraryRoot count = %d, want 0", roots)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestLibrariesRejectsMismatchedRoot(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()

	if _, err := repository.Update(
		ctx,
		testOperation("library.create"),
		func(transaction *Transaction) error {
			_, err := transaction.CreateLibrary(ctx, CreateLibrary{
				Name:           "library",
				ProfileName:    "jellyfin",
				ProfileVersion: 1,
				Creator:        Actor{Kind: ActorSystem},
				UID:            1000,
				GID:            1000,
				RootMode:       0o775,
				DirectoryMode:  0o775,
				ProjectionMode: 0o444,
				OverlayMode:    0o664,
			})
			return err
		},
	); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(
			ctx,
			"UPDATE nodes SET name = 'mismatched' WHERE kind = 'LIBRARY_ROOT'",
		); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("corrupt LibraryRoot: %v", err)
	}

	if libraries, err := repository.Libraries(ctx); !errors.Is(err, ErrInvalidLibrary) {
		t.Fatalf("Libraries error = %v, want ErrInvalidLibrary; libraries=%#v", err, libraries)
	}
}

func TestCreateLibraryValidate(t *testing.T) {
	valid := CreateLibrary{
		Name:           "library",
		ProfileName:    "jellyfin",
		ProfileVersion: 1,
		Creator:        Actor{Kind: ActorSystem},
		UID:            1000,
		GID:            1000,
		RootMode:       0o775,
		DirectoryMode:  0o775,
		ProjectionMode: 0o444,
		OverlayMode:    0o664,
	}
	tests := map[string]CreateLibrary{
		"invalid name": func() CreateLibrary {
			request := valid
			request.Name = "../library"
			return request
		}(),
		"empty Profile": func() CreateLibrary {
			request := valid
			request.ProfileName = " "
			return request
		}(),
		"invalid Profile version": func() CreateLibrary {
			request := valid
			request.ProfileVersion = 0
			return request
		}(),
		"invalid creator": func() CreateLibrary {
			request := valid
			request.Creator = Actor{}
			return request
		}(),
		"invalid root mode": func() CreateLibrary {
			request := valid
			request.RootMode = fs.ModeDir | 0o775
			return request
		}(),
		"writable Projection mode": func() CreateLibrary {
			request := valid
			request.ProjectionMode = 0o644
			return request
		}(),
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("valid CreateLibrary.Validate: %v", err)
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			if err := request.Validate(); !errors.Is(err, ErrInvalidLibrary) {
				t.Errorf("Validate error = %v, want ErrInvalidLibrary", err)
			}
		})
	}
}

func TestRepositoryUpdateRollsBackWhenOperationLogFails(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	missingScrapeID := int64(999)
	operation := testOperation("library.create")
	operation.ScrapeID = &missingScrapeID
	_, err = repository.Update(ctx, operation, func(transaction *Transaction) error {
		_, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: root.ID,
			Name:     "rolled-back-log",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		})
		return err
	})
	if err == nil {
		t.Fatal("Update with missing Scrape log reference succeeded")
	}

	assertVersionAndLogCount(t, database, 0, 0)
	var count int
	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM nodes WHERE name = 'rolled-back-log'",
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count rolled-back node: %v", err)
	}
	if count != 0 {
		t.Errorf("rolled-back node count = %d, want 0", count)
	}
}

func TestRepositoryUpdateRollsBackAfterPanic(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	panicValue := errors.New("callback panic")
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		_, _ = repository.Update(ctx, testOperation("test.panic"), func(transaction *Transaction) error {
			if _, err := transaction.CreateNode(ctx, CreateNode{
				ParentID: root.ID,
				Name:     "panicked",
				Kind:     LibraryRoot,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
					UID:  1000,
					GID:  1000,
				},
			}); err != nil {
				return err
			}
			panic(panicValue)
		})
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %v, want %v", recovered, panicValue)
	}

	assertVersionAndLogCount(t, database, 0, 0)
	var count int
	if err := database.UseConnection(ctx, func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM nodes WHERE name = 'panicked'",
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count panicked node: %v", err)
	}
	if count != 0 {
		t.Errorf("panicked node count = %d, want 0", count)
	}
	if _, err := repository.Update(
		ctx,
		testOperation("test.after-panic"),
		func(*Transaction) error { return nil },
	); err != nil {
		t.Fatalf("Update after panic: %v", err)
	}
}

func TestTransactionReadMethodsUseActiveTransaction(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	globalRoot, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	if _, err := repository.Update(ctx, testOperation("test.read"), func(transaction *Transaction) error {
		libraryRoot, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: globalRoot.ID,
			Name:     "library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		})
		if err != nil {
			return err
		}
		if root, err := transaction.GlobalRoot(ctx); err != nil {
			return err
		} else if root.ID != globalRoot.ID {
			t.Errorf("transaction GlobalRoot ID = %d, want %d", root.ID, globalRoot.ID)
		}
		if version, err := transaction.NamespaceVersion(ctx); err != nil {
			return err
		} else if version != 0 {
			t.Errorf("transaction namespace version = %d, want 0", version)
		}
		if node, err := transaction.Lookup(ctx, libraryRoot.ID, "missing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("transaction Lookup error = %v, want ErrNotFound; node=%#v", err, node)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestRepositoryUpdateRejectsInvalidOperationBeforeMutation(t *testing.T) {
	tests := map[string]OperationDetails{
		"empty paths": {
			Paths:     nil,
			NodeKinds: nil,
		},
		"length mismatch": {
			Paths:     []string{"library"},
			NodeKinds: nil,
		},
		"empty path entry": {
			Paths:     []string{""},
			NodeKinds: []NodeKind{LibraryRoot},
		},
		"invalid node kind": {
			Paths:     []string{"library"},
			NodeKinds: []NodeKind{"INVALID"},
		},
	}
	for name, details := range tests {
		t.Run(name, func(t *testing.T) {
			database := openTestDatabase(t)
			repository := NewRepository(database)
			called := false

			_, err := repository.Update(context.Background(), Operation{
				Name:    "invalid",
				Actor:   Actor{Kind: ActorSystem},
				Details: details,
			}, func(*Transaction) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrInvalidOperation) {
				t.Fatalf("Update error = %v, want ErrInvalidOperation", err)
			}
			if called {
				t.Error("Update callback was called for invalid operation")
			}
			assertVersionAndLogCount(t, database, 0, 0)
		})
	}
}

func TestRepositoryUpdateRejectsEmptyBatchUndoWithoutReference(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	called := false

	_, err := repository.Update(context.Background(), Operation{
		Name:    "projection.batch.undo",
		Actor:   Actor{Kind: ActorSystem},
		Details: OperationDetails{Paths: []string{}, NodeKinds: []NodeKind{}},
	}, func(*Transaction) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("Update error = %v, want ErrInvalidOperation", err)
	}
	if called {
		t.Error("Update callback was called for unassociated empty Batch undo")
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestRepositoryQueuesLookupDuringUpdate(t *testing.T) {
	database := openTestDatabase(t)
	writer := NewRepository(database)
	reader := NewRepository(database)
	ctx := context.Background()
	globalRoot, err := writer.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	created := make(chan int64, 1)
	release := make(chan struct{})
	updateDone := make(chan error, 1)
	transactionFailure := errors.New("rollback transaction")
	go func() {
		_, err := writer.Update(ctx, testOperation("test.concurrent"), func(transaction *Transaction) error {
			libraryRoot, err := transaction.CreateNode(ctx, CreateNode{
				ParentID: globalRoot.ID,
				Name:     "uncommitted",
				Kind:     LibraryRoot,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
					UID:  1000,
					GID:  1000,
				},
			})
			if err != nil {
				return err
			}
			created <- libraryRoot.ID
			<-release
			return transactionFailure
		})
		updateDone <- err
	}()

	libraryRootID := <-created

	cancelledContext, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := reader.Lookup(cancelledContext, libraryRootID, "missing"); !errors.Is(
		err,
		context.DeadlineExceeded,
	) {
		t.Fatalf("cancelled Lookup error = %v, want context deadline exceeded", err)
	}

	lookupDone := make(chan error, 1)
	go func() {
		_, err := reader.Lookup(ctx, libraryRootID, "missing")
		lookupDone <- err
	}()

	select {
	case err := <-lookupDone:
		t.Fatalf("queued Lookup returned before Update completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-updateDone; !errors.Is(err, transactionFailure) {
		t.Fatalf("Update error = %v, want transaction failure", err)
	}
	if err := <-lookupDone; !errors.Is(err, ErrNotFound) {
		t.Fatalf("queued Lookup error = %v, want ErrNotFound", err)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestRepositoryQueuesConcurrentUpdate(t *testing.T) {
	database := openTestDatabase(t)
	firstRepository := NewRepository(database)
	secondRepository := NewRepository(database)
	ctx := context.Background()

	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstRepository.Update(ctx, testOperation("test.first"), func(*Transaction) error {
			close(started)
			<-release
			return nil
		})
		firstDone <- err
	}()
	<-started

	secondDone := make(chan error, 1)
	go func() {
		_, err := secondRepository.Update(ctx, testOperation("test.second"), func(*Transaction) error {
			return nil
		})
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second Update returned before first completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Update: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Update: %v", err)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestRepositoryNoOpUpdateDoesNotIncrementVersionOrLog(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)

	version, err := repository.Update(
		context.Background(),
		testOperation("test.noop"),
		func(*Transaction) error {
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if version != 0 {
		t.Errorf("version = %d, want 0", version)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestManagedDirectoryNoOpsDoNotIncrementVersionOrLog(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	globalRoot, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}
	attributes := Attributes{Mode: 0o775, UID: 1000, GID: 1000}

	var libraryRootID int64
	var userDirectoryID int64
	if _, err := repository.Update(ctx, testOperation("test.create"), func(transaction *Transaction) error {
		libraryRoot, err := transaction.CreateNode(ctx, CreateNode{
			ParentID:   globalRoot.ID,
			Name:       "library",
			Kind:       LibraryRoot,
			Creator:    Actor{Kind: ActorSystem},
			Attributes: attributes,
		})
		if err != nil {
			return err
		}
		libraryRootID = libraryRoot.ID
		if _, err := transaction.EnsureManagedDirectories(
			ctx,
			libraryRoot.ID,
			[]string{"Movies"},
			attributes,
			Actor{Kind: ActorSystem},
		); err != nil {
			return err
		}
		userDirectory, err := transaction.CreateNode(ctx, CreateNode{
			ParentID:   libraryRoot.ID,
			Name:       "User",
			Kind:       Directory,
			Creator:    Actor{Kind: ActorFuse, ID: int64Pointer(1000)},
			Attributes: attributes,
		})
		userDirectoryID = userDirectory.ID
		return err
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	assertVersionAndLogCount(t, database, 1, 1)

	if version, err := repository.Update(ctx, testOperation("test.ensure"), func(transaction *Transaction) error {
		_, err := transaction.EnsureManagedDirectories(
			ctx,
			libraryRootID,
			[]string{"Movies"},
			attributes,
			Actor{Kind: ActorSystem},
		)
		return err
	}); err != nil {
		t.Fatalf("ensure existing directory: %v", err)
	} else if version != 1 {
		t.Errorf("ensure no-op version = %d, want 1", version)
	}

	if version, err := repository.Update(ctx, testOperation("test.prune"), func(transaction *Transaction) error {
		return transaction.PruneManagedDirectories(ctx, libraryRootID, userDirectoryID)
	}); err != nil {
		t.Fatalf("prune user directory: %v", err)
	} else if version != 1 {
		t.Errorf("prune no-op version = %d, want 1", version)
	}
	assertVersionAndLogCount(t, database, 1, 1)
}

func TestCreateNodeRejectsWritableProjectionMode(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	globalRoot, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	_, err = repository.Update(ctx, testOperation("test.create"), func(transaction *Transaction) error {
		libraryRoot, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: globalRoot.ID,
			Name:     "library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		})
		if err != nil {
			return err
		}
		_, err = transaction.CreateNode(ctx, CreateNode{
			ParentID: libraryRoot.ID,
			Name:     "movie.mkv",
			Kind:     ProjectionFile,
			Source: &ProjectionSource{
				Path: "/media/movie.mkv",
			},
			Creator: Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o644,
				UID:  1000,
				GID:  1000,
			},
		})
		return err
	})
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Update error = %v, want ErrInvalidNode", err)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestCreateNodeRejectsInvalidParentKinds(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	globalRoot, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	var libraryRootID int64
	var projectionID int64
	if _, err := repository.Update(ctx, testOperation("test.setup"), func(transaction *Transaction) error {
		libraryRoot, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: globalRoot.ID,
			Name:     "library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		})
		if err != nil {
			return err
		}
		libraryRootID = libraryRoot.ID

		projection, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: libraryRoot.ID,
			Name:     "movie.mkv",
			Kind:     ProjectionFile,
			Source: &ProjectionSource{
				Path: "/media/movie.mkv",
			},
			Creator: Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o444,
				UID:  1000,
				GID:  1000,
			},
		})
		projectionID = projection.ID
		return err
	}); err != nil {
		t.Fatalf("setup namespace: %v", err)
	}

	tests := map[string]CreateNode{
		"directory under GlobalRoot": {
			ParentID: globalRoot.ID,
			Name:     "directory",
			Kind:     Directory,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		},
		"LibraryRoot under LibraryRoot": {
			ParentID: libraryRootID,
			Name:     "nested-library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		},
		"directory under ProjectionFile": {
			ParentID: projectionID,
			Name:     "directory",
			Kind:     Directory,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		},
		"missing parent": {
			ParentID: 999,
			Name:     "directory",
			Kind:     Directory,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := repository.Update(
				ctx,
				testOperation("test.invalid-parent"),
				func(transaction *Transaction) error {
					_, err := transaction.CreateNode(ctx, request)
					return err
				},
			)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Update error = %v, want ErrInvalidNode", err)
			}
		})
	}
	assertVersionAndLogCount(t, database, 1, 1)
}

func TestCreateNodeRejectsGlobalRoot(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)

	_, err := repository.Update(
		context.Background(),
		testOperation("test.global-root"),
		func(transaction *Transaction) error {
			_, err := transaction.CreateNode(context.Background(), CreateNode{
				ParentID: 1,
				Name:     "root",
				Kind:     GlobalRoot,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o755,
				},
			})
			return err
		},
	)
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Update error = %v, want ErrInvalidNode", err)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestCreateNodeMapsDuplicateSiblingToConflict(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	globalRoot, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	_, err = repository.Update(ctx, testOperation("test.duplicate"), func(transaction *Transaction) error {
		request := CreateNode{
			ParentID: globalRoot.ID,
			Name:     "library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		}
		if _, err := transaction.CreateNode(ctx, request); err != nil {
			return err
		}
		_, err := transaction.CreateNode(ctx, request)
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update error = %v, want ErrConflict", err)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestLookupPersistsStableNodeIDAcrossRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := databasepkg.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	first, err := databasepkg.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	repository := NewRepository(first)
	globalRoot, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	var libraryRootID int64
	var directoryID int64
	var projectionID int64
	if _, err := repository.Update(ctx, testOperation("test.create"), func(transaction *Transaction) error {
		libraryRoot, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: globalRoot.ID,
			Name:     "library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
				UID:  1000,
				GID:  1000,
			},
		})
		if err != nil {
			return err
		}
		libraryRootID = libraryRoot.ID

		directory, err := transaction.EnsureManagedDirectories(
			ctx,
			libraryRoot.ID,
			[]string{"Movies", "Dune"},
			Attributes{Mode: 0o775, UID: 1000, GID: 1000},
			Actor{Kind: ActorSystem},
		)
		if err != nil {
			return err
		}
		directoryID = directory.ID

		projection, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: directory.ID,
			Name:     "Dune.mkv",
			Kind:     ProjectionFile,
			Source: &ProjectionSource{
				Path:          "/media/Dune.mkv",
				DeviceID:      11,
				Inode:         22,
				Size:          33,
				AtimeNS:       44,
				MtimeNS:       55,
				SourceCtimeNS: 66,
				VerifiedAtNS:  77,
			},
			Creator: Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o444,
				UID:  1000,
				GID:  1000,
			},
		})
		projectionID = projection.ID
		return err
	}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	assertVersionAndLogCount(t, first, 1, 1)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := databasepkg.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer second.Close()

	node, err := NewRepository(second).Lookup(ctx, libraryRootID, "Movies/Dune")
	if err != nil {
		t.Fatalf("Lookup after restart: %v", err)
	}
	if node.ID != directoryID {
		t.Errorf("node ID after restart = %d, want %d", node.ID, directoryID)
	}
	if node.Kind != Directory || !node.ManagedDirectory {
		t.Errorf("node after restart = %#v, want managed directory", node)
	}

	projection, err := NewRepository(second).Lookup(
		ctx,
		libraryRootID,
		"Movies/Dune/Dune.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup Projection after restart: %v", err)
	}
	if projection.ID != projectionID {
		t.Errorf("Projection ID after restart = %d, want %d", projection.ID, projectionID)
	}
	if projection.Source == nil ||
		projection.Source.Path != "/media/Dune.mkv" ||
		projection.Source.DeviceID != 11 ||
		projection.Source.Inode != 22 ||
		projection.Source.Size != 33 {
		t.Errorf("Projection source after restart = %#v", projection.Source)
	}
}

func TestPruneManagedDirectoriesStopsAtUserDirectoryAndLibraryRoot(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	ctx := context.Background()
	globalRoot, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	var libraryRootID int64
	var userLeafID int64
	var managedLeafID int64
	attributes := Attributes{Mode: 0o775, UID: 1000, GID: 1000}
	if _, err := repository.Update(ctx, testOperation("test.create"), func(transaction *Transaction) error {
		libraryRoot, err := transaction.CreateNode(ctx, CreateNode{
			ParentID:   globalRoot.ID,
			Name:       "library",
			Kind:       LibraryRoot,
			Creator:    Actor{Kind: ActorSystem},
			Attributes: attributes,
		})
		if err != nil {
			return err
		}
		libraryRootID = libraryRoot.ID

		userDirectory, err := transaction.CreateNode(ctx, CreateNode{
			ParentID:   libraryRoot.ID,
			Name:       "User",
			Kind:       Directory,
			Creator:    Actor{Kind: ActorFuse, ID: int64Pointer(1000)},
			Attributes: attributes,
		})
		if err != nil {
			return err
		}
		userLeaf, err := transaction.EnsureManagedDirectories(
			ctx,
			userDirectory.ID,
			[]string{"Leaf"},
			attributes,
			Actor{Kind: ActorSystem},
		)
		if err != nil {
			return err
		}
		userLeafID = userLeaf.ID

		managedLeaf, err := transaction.EnsureManagedDirectories(
			ctx,
			libraryRoot.ID,
			[]string{"Managed", "Leaf"},
			attributes,
			Actor{Kind: ActorSystem},
		)
		managedLeafID = managedLeaf.ID
		return err
	}); err != nil {
		t.Fatalf("create directories: %v", err)
	}

	if _, err := repository.Update(ctx, testOperation("test.prune"), func(transaction *Transaction) error {
		if err := transaction.PruneManagedDirectories(ctx, libraryRootID, userLeafID); err != nil {
			return err
		}
		return transaction.PruneManagedDirectories(ctx, libraryRootID, managedLeafID)
	}); err != nil {
		t.Fatalf("prune directories: %v", err)
	}

	if _, err := repository.Lookup(ctx, libraryRootID, "User"); err != nil {
		t.Fatalf("Lookup user directory: %v", err)
	}
	for _, path := range []string{"User/Leaf", "Managed", "Managed/Leaf"} {
		if node, err := repository.Lookup(ctx, libraryRootID, path); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lookup %q error = %v, want ErrNotFound; node=%#v", path, err, node)
		}
	}
	assertVersionAndLogCount(t, database, 2, 2)
}

func TestValidateRelativePath(t *testing.T) {
	valid := map[string][]string{
		"movie.mkv":       {"movie.mkv"},
		"Movies/Dune.mkv": {"Movies", "Dune.mkv"},
		"媒体/电影.mkv":       {"媒体", "电影.mkv"},
		`backslash\\name`: {`backslash\\name`},
	}
	for value, want := range valid {
		t.Run("valid_"+value, func(t *testing.T) {
			got, err := ValidateRelativePath(value)
			if err != nil {
				t.Fatalf("ValidateRelativePath: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("components = %#v, want %#v", got, want)
			}
		})
	}

	invalid := []string{
		"",
		"/absolute",
		"trailing/",
		"empty//component",
		"./movie",
		"Movies/./movie",
		"../movie",
		"Movies/../movie",
		"nul\x00name",
		string([]byte{0xff}),
		strings.Repeat("a", 256),
		strings.Repeat("a/", 2048) + "a",
	}
	for _, value := range invalid {
		t.Run("invalid", func(t *testing.T) {
			if _, err := ValidateRelativePath(value); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("ValidateRelativePath(%q) error = %v, want ErrInvalidPath", value, err)
			}
		})
	}
}

func testOperation(name string) Operation {
	return Operation{
		Name:  name,
		Actor: Actor{Kind: ActorSystem},
		Details: OperationDetails{
			Paths:     []string{"library"},
			NodeKinds: []NodeKind{LibraryRoot},
		},
	}
}

func openTestDatabase(t *testing.T) *databasepkg.DB {
	t.Helper()

	root := t.TempDir()
	opened, err := databasepkg.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		databasepkg.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return opened
}

func assertVersionAndLogCount(t *testing.T, opened *databasepkg.DB, wantVersion, wantLogs int64) {
	t.Helper()

	var version int64
	if err := opened.UseConnection(context.Background(), func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT version FROM namespace_state WHERE singleton = 1",
		).Scan(&version)
	}); err != nil {
		t.Fatalf("read namespace version: %v", err)
	}
	if version != wantVersion {
		t.Errorf("namespace version = %d, want %d", version, wantVersion)
	}

	var logs int64
	if err := opened.UseConnection(context.Background(), func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM operation_log",
		).Scan(&logs)
	}); err != nil {
		t.Fatalf("count operation log: %v", err)
	}
	if logs != wantLogs {
		t.Errorf("operation log count = %d, want %d", logs, wantLogs)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
