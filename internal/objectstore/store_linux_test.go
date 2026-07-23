package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

func TestOpenPreparesPrivateObjectStoreLayout(t *testing.T) {
	fixture := newFixture(t)

	for _, path := range []string{
		fixture.storePath,
		filepath.Join(fixture.storePath, stagingDirectory),
		filepath.Join(fixture.storePath, liveDirectory),
	} {
		assertPathOwnerAndMode(t, path, 0o700)
	}
}

func TestOpenRejectsUnsafeObjectStoreLayout(t *testing.T) {
	t.Run("relative path", func(t *testing.T) {
		opened := openTestDatabase(t, t.TempDir())
		if store, err := Open("objects", opened); err == nil {
			store.Close()
			t.Fatal("Open succeeded")
		}
	})

	t.Run("root symlink", func(t *testing.T) {
		root := t.TempDir()
		opened := openTestDatabase(t, root)
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatalf("Mkdir target: %v", err)
		}
		storePath := filepath.Join(root, "objects")
		if err := os.Symlink(target, storePath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		if store, err := Open(storePath, opened); err == nil {
			store.Close()
			t.Fatal("Open succeeded")
		}
	})

	t.Run("staging symlink", func(t *testing.T) {
		root := t.TempDir()
		opened := openTestDatabase(t, root)
		storePath := filepath.Join(root, "objects")
		target := filepath.Join(root, "target")
		mkdirPrivate(t, storePath)
		mkdirPrivate(t, target)
		if err := os.Symlink(target, filepath.Join(storePath, stagingDirectory)); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		if store, err := Open(storePath, opened); err == nil {
			store.Close()
			t.Fatal("Open succeeded")
		}
	})

	t.Run("unexpected live type", func(t *testing.T) {
		root := t.TempDir()
		opened := openTestDatabase(t, root)
		storePath := filepath.Join(root, "objects")
		mkdirPrivate(t, storePath)
		mkdirPrivate(t, filepath.Join(storePath, stagingDirectory))
		if err := unix.Mkfifo(filepath.Join(storePath, liveDirectory), 0o600); err != nil {
			t.Fatalf("Mkfifo: %v", err)
		}
		if store, err := Open(storePath, opened); err == nil {
			store.Close()
			t.Fatal("Open succeeded")
		}
	})

	t.Run("object symlink", func(t *testing.T) {
		root := t.TempDir()
		opened := openTestDatabase(t, root)
		storePath := filepath.Join(root, "objects")
		mkdirPrivate(t, storePath)
		mkdirPrivate(t, filepath.Join(storePath, stagingDirectory))
		mkdirPrivate(t, filepath.Join(storePath, liveDirectory))
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatalf("WriteFile target: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(storePath, liveDirectory, "1")); err != nil {
			t.Fatalf("Symlink object: %v", err)
		}
		if store, err := Open(storePath, opened); err == nil {
			store.Close()
			t.Fatal("Open succeeded")
		}
	})

	t.Run("object FIFO", func(t *testing.T) {
		root := t.TempDir()
		opened := openTestDatabase(t, root)
		storePath := filepath.Join(root, "objects")
		mkdirPrivate(t, storePath)
		mkdirPrivate(t, filepath.Join(storePath, stagingDirectory))
		mkdirPrivate(t, filepath.Join(storePath, liveDirectory))
		if err := unix.Mkfifo(filepath.Join(storePath, liveDirectory, "1"), 0o600); err != nil {
			t.Fatalf("Mkfifo object: %v", err)
		}
		if store, err := Open(storePath, opened); err == nil {
			store.Close()
			t.Fatal("Open succeeded")
		}
	})
}

func TestCreateDurablyPromotesObjectAndCreatesOverlay(t *testing.T) {
	fixture := newFixture(t)
	content := []byte("poster content")

	handle, node, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		bytes.NewReader(content),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer handle.Close()

	if node.Kind != namespace.OverlayFile ||
		node.ObjectID == nil ||
		*node.ObjectID != handle.ObjectID {
		t.Errorf("created node = %#v, handle object ID = %d", node, handle.ObjectID)
	}
	got, err := io.ReadAll(handle)
	if err != nil {
		t.Fatalf("ReadAll returned handle: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("returned content = %q, want %q", got, content)
	}

	objectName := strconv.FormatInt(handle.ObjectID, 10)
	if _, err := os.Stat(filepath.Join(fixture.storePath, stagingDirectory, objectName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging object stat error = %v, want not exist", err)
	}
	livePath := filepath.Join(fixture.storePath, liveDirectory, objectName)
	assertPathOwnerAndMode(t, livePath, 0o600)
	persisted, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatalf("ReadFile live object: %v", err)
	}
	if !bytes.Equal(persisted, content) {
		t.Errorf("live content = %q, want %q", persisted, content)
	}
	state, _, liveAt, _ := readObject(t, fixture.database, handle.ObjectID)
	if state != stateLive || liveAt == nil {
		t.Errorf("object state = %q live_at=%v, want LIVE with timestamp", state, liveAt)
	}
	found, err := fixture.repository.Lookup(
		context.Background(),
		fixture.libraryRootID,
		"poster.jpg",
	)
	if err != nil {
		t.Fatalf("Lookup Overlay: %v", err)
	}
	if found.ID != node.ID || found.ObjectID == nil || *found.ObjectID != handle.ObjectID {
		t.Errorf("persisted node = %#v, want %#v", found, node)
	}
	assertNamespaceState(t, fixture.database, 2, 2)
}

func TestCreateCrashPointsNeverExposeOverlayNode(t *testing.T) {
	crash := errors.New("simulated crash")
	tests := map[string]struct {
		install       func(*Store)
		wantStaging   bool
		wantLive      bool
		wantObjectRow bool
	}{
		"after staging record": {
			install: func(store *Store) {
				store.hooks.afterStagingRecord = func(int64) error { return crash }
			},
			wantObjectRow: true,
		},
		"after staging sync": {
			install: func(store *Store) {
				store.hooks.afterStagingSync = func(int64) error { return crash }
			},
			wantObjectRow: true,
			wantStaging:   true,
		},
		"after rename": {
			install: func(store *Store) {
				store.hooks.afterRename = func(int64) error { return crash }
			},
			wantObjectRow: true,
			wantLive:      true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t)
			test.install(fixture.store)
			handle, node, err := fixture.store.Create(
				context.Background(),
				overlayOperation("library/poster.jpg"),
				overlayRequest(fixture.libraryRootID, "poster.jpg"),
				strings.NewReader("content"),
			)
			if handle != nil {
				handle.Close()
			}
			if !errors.Is(err, crash) {
				t.Fatalf("Create error = %v, want simulated crash", err)
			}
			if node.ID != 0 {
				t.Errorf("created node = %#v, want zero value", node)
			}
			if found, lookupErr := fixture.repository.Lookup(
				context.Background(),
				fixture.libraryRootID,
				"poster.jpg",
			); !errors.Is(lookupErr, namespace.ErrNotFound) {
				t.Errorf("Lookup error = %v, want ErrNotFound; node=%#v", lookupErr, found)
			}

			objectID, exists := latestObjectID(t, fixture.database)
			if exists != test.wantObjectRow {
				t.Fatalf("object row exists = %v, want %v", exists, test.wantObjectRow)
			}
			if exists {
				state, _, liveAt, _ := readObject(t, fixture.database, objectID)
				if state != stateStaging || liveAt != nil {
					t.Errorf("object lifecycle = %q/%v, want STAGING/NULL", state, liveAt)
				}
				objectName := strconv.FormatInt(objectID, 10)
				assertExists(t, filepath.Join(fixture.storePath, stagingDirectory, objectName), test.wantStaging)
				assertExists(t, filepath.Join(fixture.storePath, liveDirectory, objectName), test.wantLive)
			}
			assertNamespaceState(t, fixture.database, 1, 1)

			if err := fixture.store.Close(); err != nil {
				t.Fatalf("Close before restart: %v", err)
			}
			reopened, err := Open(fixture.storePath, fixture.database)
			if err != nil {
				t.Fatalf("Open after simulated crash: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatalf("Close after simulated crash: %v", err)
			}
		})
	}
}

func TestCreateFilesystemFailureLeavesNoVisibleNode(t *testing.T) {
	fixture := newFixture(t)
	writeFailure := errors.New("write failed")

	handle, node, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		&failingReader{err: writeFailure},
	)
	if handle != nil {
		handle.Close()
	}
	if !errors.Is(err, writeFailure) {
		t.Fatalf("Create error = %v, want write failure", err)
	}
	if node.ID != 0 {
		t.Errorf("created node = %#v, want zero value", node)
	}
	if found, lookupErr := fixture.repository.Lookup(
		context.Background(),
		fixture.libraryRootID,
		"poster.jpg",
	); !errors.Is(lookupErr, namespace.ErrNotFound) {
		t.Errorf("Lookup error = %v, want ErrNotFound; node=%#v", lookupErr, found)
	}
	objectID, exists := latestObjectID(t, fixture.database)
	if !exists {
		t.Fatal("STAGING object row was not retained")
	}
	state, _, liveAt, _ := readObject(t, fixture.database, objectID)
	if state != stateStaging || liveAt != nil {
		t.Errorf("object lifecycle = %q/%v, want STAGING/NULL", state, liveAt)
	}
	assertNamespaceState(t, fixture.database, 1, 1)
}

func TestCreateHandlePreparationFailureLeavesNoVisibleNode(t *testing.T) {
	fixture := newFixture(t)
	prepareFailure := errors.New("prepare handle failed")
	fixture.store.hooks.prepareHandle = func(*File) error {
		return prepareFailure
	}

	handle, node, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		strings.NewReader("content"),
	)
	if handle != nil {
		handle.Close()
	}
	if !errors.Is(err, prepareFailure) {
		t.Fatalf("Create error = %v, want handle preparation failure", err)
	}
	if node.ID != 0 {
		t.Errorf("created node = %#v, want zero value", node)
	}
	if found, lookupErr := fixture.repository.Lookup(
		context.Background(),
		fixture.libraryRootID,
		"poster.jpg",
	); !errors.Is(lookupErr, namespace.ErrNotFound) {
		t.Errorf("Lookup error = %v, want ErrNotFound; node=%#v", lookupErr, found)
	}
	objectID, exists := latestObjectID(t, fixture.database)
	if !exists {
		t.Fatal("STAGING object row was not retained")
	}
	state, _, liveAt, _ := readObject(t, fixture.database, objectID)
	if state != stateStaging || liveAt != nil {
		t.Errorf("object lifecycle = %q/%v, want STAGING/NULL", state, liveAt)
	}
	if _, err := os.Stat(filepath.Join(
		fixture.storePath,
		liveDirectory,
		strconv.FormatInt(objectID, 10),
	)); err != nil {
		t.Fatalf("stat promoted orphan: %v", err)
	}
	assertNamespaceState(t, fixture.database, 1, 1)
}

func TestCreateDatabaseRollbackLeavesNoBrokenOverlayNode(t *testing.T) {
	fixture := newFixture(t)

	first, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		strings.NewReader("first"),
	)
	if err != nil {
		t.Fatalf("create first object: %v", err)
	}
	defer first.Close()

	second, node, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		strings.NewReader("second"),
	)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, namespace.ErrConflict) {
		t.Fatalf("create conflicting object error = %v, want ErrConflict", err)
	}
	if node.ID != 0 {
		t.Errorf("conflicting node = %#v, want zero value", node)
	}

	secondObjectID, exists := latestObjectID(t, fixture.database)
	if !exists || secondObjectID == first.ObjectID {
		t.Fatalf("latest object ID = %d exists=%v, want retained second object", secondObjectID, exists)
	}
	state, _, liveAt, _ := readObject(t, fixture.database, secondObjectID)
	if state != stateStaging || liveAt != nil {
		t.Errorf("rolled-back lifecycle = %q/%v, want STAGING/NULL", state, liveAt)
	}
	if _, err := os.Stat(filepath.Join(
		fixture.storePath,
		liveDirectory,
		strconv.FormatInt(secondObjectID, 10),
	)); err != nil {
		t.Fatalf("stat promoted orphan: %v", err)
	}
	assertNamespaceState(t, fixture.database, 2, 2)
}

func TestOpenObjectReturnsEIOForMissingLiveObjectWithoutReplacement(t *testing.T) {
	fixture := newFixture(t)
	handle, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		strings.NewReader("content"),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	objectID := handle.ObjectID
	if err := handle.Close(); err != nil {
		t.Fatalf("Close created handle: %v", err)
	}

	livePath := filepath.Join(fixture.storePath, liveDirectory, strconv.FormatInt(objectID, 10))
	if err := os.Remove(livePath); err != nil {
		t.Fatalf("Remove live object: %v", err)
	}
	opened, err := fixture.store.OpenObject(context.Background(), objectID, unix.O_RDONLY)
	if opened != nil {
		opened.Close()
	}
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("OpenObject error = %v, want EIO", err)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live object stat error = %v, want not exist", err)
	}
}

func TestDeleteObjectKeepsOpenedDescriptorsValid(t *testing.T) {
	fixture := newFixture(t)
	content := []byte("content")
	created, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		bytes.NewReader(content),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer created.Close()
	opened, err := fixture.store.OpenObject(context.Background(), created.ObjectID, unix.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	defer opened.Close()

	if err := fixture.store.DeleteObject(created.ObjectID); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	for name, file := range map[string]*File{
		"created": created,
		"opened":  opened,
	} {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("%s Seek: %v", name, err)
		}
		got, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("%s ReadAll: %v", name, err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("%s content = %q, want %q", name, got, content)
		}
	}
	reopened, err := fixture.store.OpenObject(context.Background(), created.ObjectID, unix.O_RDONLY)
	if reopened != nil {
		reopened.Close()
	}
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("OpenObject after delete error = %v, want EIO", err)
	}
}

func TestStoreUsesHeldDirectoriesAfterConfiguredPathReplacement(t *testing.T) {
	fixture := newFixture(t)
	originalPath := fixture.storePath + "-original"
	attackerPath := filepath.Join(fixture.root, "attacker")
	mkdirPrivate(t, attackerPath)
	mkdirPrivate(t, filepath.Join(attackerPath, stagingDirectory))
	mkdirPrivate(t, filepath.Join(attackerPath, liveDirectory))
	if err := os.Rename(fixture.storePath, originalPath); err != nil {
		t.Fatalf("Rename store: %v", err)
	}
	if err := os.Symlink(attackerPath, fixture.storePath); err != nil {
		t.Fatalf("replace store with symlink: %v", err)
	}

	handle, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster.jpg"),
		overlayRequest(fixture.libraryRootID, "poster.jpg"),
		strings.NewReader("content"),
	)
	if err != nil {
		t.Fatalf("Create after replacement: %v", err)
	}
	defer handle.Close()
	objectName := strconv.FormatInt(handle.ObjectID, 10)
	if _, err := os.Stat(filepath.Join(originalPath, liveDirectory, objectName)); err != nil {
		t.Fatalf("stat original live object: %v", err)
	}
	if _, err := os.Stat(filepath.Join(attackerPath, liveDirectory, objectName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attacker live object stat error = %v, want not exist", err)
	}
}

func TestRestartPreservesLiveObjectIdentityAndContents(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	lockPath := filepath.Join(root, "run", "scrapefsd.lock")
	storePath := filepath.Join(root, "objects")
	ctx := context.Background()

	firstDatabase, err := database.OpenWithOptions(
		ctx,
		databasePath,
		database.Options{LockPath: lockPath},
	)
	if err != nil {
		t.Fatalf("first database OpenWithOptions: %v", err)
	}
	firstStore, err := Open(storePath, firstDatabase)
	if err != nil {
		firstDatabase.Close()
		t.Fatalf("first store Open: %v", err)
	}
	firstRepository := namespace.NewRepository(firstDatabase)
	libraryRootID := createLibraryRoot(t, firstRepository)
	content := []byte("persistent content")
	handle, node, err := firstStore.Create(
		ctx,
		overlayOperation("library/poster.jpg"),
		overlayRequest(libraryRootID, "poster.jpg"),
		bytes.NewReader(content),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	objectID := handle.ObjectID
	nodeID := node.ID
	if err := handle.Close(); err != nil {
		t.Fatalf("close object handle: %v", err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	if err := firstDatabase.Close(); err != nil {
		t.Fatalf("close first database: %v", err)
	}

	secondDatabase, err := database.OpenWithOptions(
		ctx,
		databasePath,
		database.Options{LockPath: lockPath},
	)
	if err != nil {
		t.Fatalf("second database OpenWithOptions: %v", err)
	}
	defer secondDatabase.Close()
	secondStore, err := Open(storePath, secondDatabase)
	if err != nil {
		t.Fatalf("second store Open: %v", err)
	}
	defer secondStore.Close()

	found, err := namespace.NewRepository(secondDatabase).Lookup(ctx, libraryRootID, "poster.jpg")
	if err != nil {
		t.Fatalf("Lookup after restart: %v", err)
	}
	if found.ID != nodeID || found.ObjectID == nil || *found.ObjectID != objectID {
		t.Errorf("node after restart = %#v, want node %d object %d", found, nodeID, objectID)
	}
	reopened, err := secondStore.OpenObject(ctx, objectID, unix.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenObject after restart: %v", err)
	}
	defer reopened.Close()
	got, err := io.ReadAll(reopened)
	if err != nil {
		t.Fatalf("ReadAll after restart: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content after restart = %q, want %q", got, content)
	}
}

type fixture struct {
	root          string
	storePath     string
	database      *database.DB
	store         *Store
	repository    *namespace.Repository
	libraryRootID int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	opened := openTestDatabase(t, root)
	storePath := filepath.Join(root, "objects")
	store, err := Open(storePath, opened)
	if err != nil {
		t.Fatalf("Open object store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close object store: %v", err)
		}
	})
	repository := namespace.NewRepository(opened)
	return fixture{
		root:          root,
		storePath:     storePath,
		database:      opened,
		store:         store,
		repository:    repository,
		libraryRootID: createLibraryRoot(t, repository),
	}
}

func openTestDatabase(t *testing.T, root string) *database.DB {
	t.Helper()
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
			t.Errorf("Close database: %v", err)
		}
	})
	return opened
}

func createLibraryRoot(t *testing.T, repository *namespace.Repository) int64 {
	t.Helper()
	ctx := context.Background()
	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}
	var libraryRootID int64
	if _, err := repository.Update(
		ctx,
		namespace.Operation{
			Name:  "library.create",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{"library"},
				NodeKinds: []namespace.NodeKind{namespace.LibraryRoot},
			},
		},
		func(transaction *namespace.Transaction) error {
			node, err := transaction.CreateNode(ctx, namespace.CreateNode{
				ParentID: root.ID,
				Name:     "library",
				Kind:     namespace.LibraryRoot,
				Creator:  namespace.Actor{Kind: namespace.ActorSystem},
				Attributes: namespace.Attributes{
					Mode: 0o775,
				},
			})
			libraryRootID = node.ID
			return err
		},
	); err != nil {
		t.Fatalf("create LibraryRoot: %v", err)
	}
	return libraryRootID
}

func overlayOperation(path string) namespace.Operation {
	return namespace.Operation{
		Name:  "overlay.create",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
		Details: namespace.OperationDetails{
			Paths:     []string{path},
			NodeKinds: []namespace.NodeKind{namespace.OverlayFile},
		},
	}
}

func overlayRequest(parentID int64, name string) namespace.CreateOverlay {
	return namespace.CreateOverlay{
		ParentID: parentID,
		Name:     name,
		Creator:  namespace.Actor{Kind: namespace.ActorSystem},
		Attributes: namespace.Attributes{
			Mode: 0o600,
		},
	}
}

func readObject(
	t *testing.T,
	opened *database.DB,
	objectID int64,
) (string, int64, *int64, int64) {
	t.Helper()
	var state string
	var createdAt int64
	var liveAt *int64
	var updatedAt int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(context.Background(), `
			SELECT state, created_at_ns, live_at_ns, updated_at_ns
			FROM objects
			WHERE id = ?
		`, objectID).Scan(&state, &createdAt, &liveAt, &updatedAt)
	}); err != nil {
		t.Fatalf("read object %d: %v", objectID, err)
	}
	return state, createdAt, liveAt, updatedAt
}

func latestObjectID(t *testing.T, opened *database.DB) (int64, bool) {
	t.Helper()
	var objectID *int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT MAX(id) FROM objects",
		).Scan(&objectID)
	}); err != nil {
		t.Fatalf("read latest object ID: %v", err)
	}
	if objectID == nil {
		return 0, false
	}
	return *objectID, true
}

func assertNamespaceState(t *testing.T, opened *database.DB, wantVersion, wantLogs int64) {
	t.Helper()
	var version int64
	var logs int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		if err := connection.QueryRowContext(
			context.Background(),
			"SELECT version FROM namespace_state WHERE singleton = 1",
		).Scan(&version); err != nil {
			return err
		}
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM operation_log",
		).Scan(&logs)
	}); err != nil {
		t.Fatalf("read namespace state: %v", err)
	}
	if version != wantVersion || logs != wantLogs {
		t.Errorf(
			"namespace state = version %d logs %d, want %d/%d",
			version,
			logs,
			wantVersion,
			wantLogs,
		)
	}
}

func mkdirPrivate(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir(%q): %v", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("Chmod(%q): %v", path, err)
	}
}

func assertPathOwnerAndMode(t *testing.T, path string, wantMode os.FileMode) {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		t.Errorf(
			"%s owner = %d:%d, want %d:%d",
			path,
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}
	if got := os.FileMode(stat.Mode).Perm(); got != wantMode {
		t.Errorf("%s mode = %#o, want %#o", path, got, wantMode)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if want && err != nil {
		t.Errorf("Stat(%q): %v", path, err)
	}
	if !want && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(%q) error = %v, want not exist", path, err)
	}
}

type failingReader struct {
	err  error
	read bool
}

func (reader *failingReader) Read(buffer []byte) (int, error) {
	if reader.read {
		return 0, reader.err
	}
	reader.read = true
	copy(buffer, "partial")
	return len("partial"), reader.err
}
