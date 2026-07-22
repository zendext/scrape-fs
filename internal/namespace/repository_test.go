package namespace

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
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
	if err := database.Conn().QueryRowContext(ctx, `
		SELECT operation, actor_kind, actor_id, namespace_version, details_json
		FROM operation_log
	`).Scan(&operation, &actorKind, &actorID, &loggedVersion, &details); err != nil {
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
	if err := database.Conn().QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM nodes WHERE name = 'rolled-back'",
	).Scan(&count); err != nil {
		t.Fatalf("count rolled-back node: %v", err)
	}
	if count != 0 {
		t.Errorf("rolled-back node count = %d, want 0", count)
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
	if err := database.Conn().QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM nodes WHERE name = 'rolled-back-log'",
	).Scan(&count); err != nil {
		t.Fatalf("count rolled-back node: %v", err)
	}
	if count != 0 {
		t.Errorf("rolled-back node count = %d, want 0", count)
	}
}

func TestRepositoryUpdateRejectsInvalidOperationBeforeMutation(t *testing.T) {
	database := openTestDatabase(t)
	repository := NewRepository(database)
	called := false

	_, err := repository.Update(context.Background(), Operation{
		Name:  "invalid",
		Actor: Actor{Kind: ActorSystem},
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
}

func TestRepositorySerializesLookupWithUpdate(t *testing.T) {
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
	lookupDone := make(chan error, 1)
	go func() {
		_, err := reader.Lookup(ctx, libraryRootID, "missing")
		lookupDone <- err
	}()

	select {
	case err := <-lookupDone:
		t.Fatalf("Lookup returned before transaction completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-updateDone; !errors.Is(err, transactionFailure) {
		t.Fatalf("Update error = %v, want transaction failure", err)
	}
	if err := <-lookupDone; !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup error = %v, want ErrNotFound", err)
	}
	assertVersionAndLogCount(t, database, 0, 0)
}

func TestRepositorySerializesConcurrentUpdates(t *testing.T) {
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

func TestLookupPersistsStableNodeIDAcrossRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	first, err := database.OpenWithOptions(ctx, databasePath, options)
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

	second, err := database.OpenWithOptions(ctx, databasePath, options)
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

func openTestDatabase(t *testing.T) *database.DB {
	t.Helper()

	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
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

func assertVersionAndLogCount(t *testing.T, opened *database.DB, wantVersion, wantLogs int64) {
	t.Helper()

	var version int64
	if err := opened.Conn().QueryRowContext(
		context.Background(),
		"SELECT version FROM namespace_state WHERE singleton = 1",
	).Scan(&version); err != nil {
		t.Fatalf("read namespace version: %v", err)
	}
	if version != wantVersion {
		t.Errorf("namespace version = %d, want %d", version, wantVersion)
	}

	var logs int64
	if err := opened.Conn().QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM operation_log",
	).Scan(&logs); err != nil {
		t.Fatalf("count operation log: %v", err)
	}
	if logs != wantLogs {
		t.Errorf("operation log count = %d, want %d", logs, wantLogs)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
