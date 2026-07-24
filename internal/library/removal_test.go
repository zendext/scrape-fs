package library

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestServiceRemoveEmptyLibraryRetiresAtomically(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")

	retired, err := service.Remove(ctx, RemoveRequest{
		Name:  "media",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if retired.ID != created.ID ||
		retired.Name != created.Name ||
		retired.Status != namespace.LibraryRetired ||
		retired.RootNodeID != 0 {
		t.Errorf("retired Library = %#v, created = %#v", retired, created)
	}
	if _, err := service.Get(ctx, "media"); !errors.Is(err, namespace.ErrLibraryNotFound) {
		t.Errorf("Get retired Library error = %v, want ErrLibraryNotFound", err)
	}
	if _, err := namespace.NewRepository(opened).Node(ctx, created.RootNodeID); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("read retired LibraryRoot error = %v, want ErrNotFound", err)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
	assertLastOperation(t, opened, "library.remove", 2)
}

func TestServiceRemoveNonEmptyLibraryRequiresRecursive(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	service := NewService(repository)
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")
	var child namespace.Node
	if _, err := repository.Update(
		ctx,
		namespace.Operation{
			Name:  "test.directory.create",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{"media/Movies"},
				NodeKinds: []namespace.NodeKind{namespace.Directory},
			},
		},
		func(transaction *namespace.Transaction) error {
			var err error
			child, err = transaction.CreateNode(ctx, namespace.CreateNode{
				ParentID: created.RootNodeID,
				Name:     "Movies",
				Kind:     namespace.Directory,
				Creator:  namespace.Actor{Kind: namespace.ActorSystem},
				Attributes: namespace.Attributes{
					Mode: 0o775,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create child: %v", err)
	}

	if _, err := service.Remove(ctx, RemoveRequest{
		Name:  "media",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
	}); !errors.Is(err, namespace.ErrNotEmpty) {
		t.Fatalf("Remove non-empty error = %v, want ErrNotEmpty", err)
	}
	found, err := service.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get after rejected removal: %v", err)
	}
	if found != created {
		t.Errorf("Library after rejected removal = %#v, want %#v", found, created)
	}
	if _, err := repository.Node(ctx, child.ID); err != nil {
		t.Errorf("read child after rejected removal: %v", err)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
}

func TestServiceRemoveRecursivePreservesSourcesAndReclaimsOverlays(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	now := time.Now().UTC()
	opened, err := database.OpenWithOptions(
		ctx,
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()
	storePath := filepath.Join(root, "objects")
	store, err := objectstore.OpenWithOptions(storePath, opened, objectstore.Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Open object store: %v", err)
	}
	defer store.Close()
	repository := namespace.NewRepository(opened)
	service := NewServiceWithObjectReclaimer(repository, store)
	created := createTestLibrary(t, service, "media", "jellyfin")

	directSource := filepath.Join(root, "direct.mkv")
	directContent := []byte("direct source")
	if err := os.WriteFile(directSource, directContent, 0o600); err != nil {
		t.Fatalf("write direct source: %v", err)
	}
	if _, err := projection.NewService(repository).Create(ctx, projection.CreateRequest{
		SourcePath: directSource,
		Target:     "media:/Movies/Direct.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create direct Projection: %v", err)
	}

	batchSource := filepath.Join(root, "batch.mkv")
	batchContent := []byte("batch source")
	if err := os.WriteFile(batchSource, batchContent, 0o600); err != nil {
		t.Fatalf("write batch source: %v", err)
	}
	batchData := []byte(`{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"` +
		batchSource + `","target":"Shows/Batch.mkv"}]}`)
	batchService := projectionbatch.NewService(repository)
	validated, err := batchService.Validate(ctx, batchData)
	if err != nil {
		t.Fatalf("Validate batch: %v", err)
	}
	applied, err := batchService.Apply(ctx, projectionbatch.ApplyRequest{
		Data:           batchData,
		ExpectedDigest: validated.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         projectionbatch.Caller{UID: 1000, GID: 1000},
	})
	if err != nil {
		t.Fatalf("Apply batch: %v", err)
	}

	closedHandle, _, err := store.Create(
		ctx,
		testOverlayOperation("media/closed.bin"),
		testOverlayRequest(created.RootNodeID, "closed.bin"),
		bytes.NewReader([]byte("closed")),
	)
	if err != nil {
		t.Fatalf("Create closed Overlay: %v", err)
	}
	closedObjectID := closedHandle.ObjectID
	if err := closedHandle.Close(); err != nil {
		t.Fatalf("Close closed Overlay: %v", err)
	}
	activeHandle, _, err := store.Create(
		ctx,
		testOverlayOperation("media/active.bin"),
		testOverlayRequest(created.RootNodeID, "active.bin"),
		bytes.NewReader([]byte("active")),
	)
	if err != nil {
		t.Fatalf("Create active Overlay: %v", err)
	}
	activeObjectID := activeHandle.ObjectID
	now = now.Add(2 * time.Hour)

	retired, err := service.Remove(ctx, RemoveRequest{
		Name:      "media",
		Recursive: true,
		Actor:     namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Remove recursive: %v", err)
	}
	if retired.Status != namespace.LibraryRetired || retired.RootNodeID != 0 {
		t.Errorf("retired Library = %#v", retired)
	}
	if _, err := repository.Node(ctx, created.RootNodeID); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("read removed LibraryRoot error = %v, want ErrNotFound", err)
	}
	assertFileContent(t, directSource, directContent)
	assertFileContent(t, batchSource, batchContent)
	assertStoredObject(t, opened, storePath, closedObjectID, false)
	assertStoredObject(t, opened, storePath, activeObjectID, true)
	if _, err := activeHandle.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek active Overlay: %v", err)
	}
	if content, err := io.ReadAll(activeHandle); err != nil {
		t.Fatalf("read active Overlay: %v", err)
	} else if string(content) != "active" {
		t.Errorf("active Overlay content = %q, want active", content)
	}

	listed, err := batchService.List(ctx)
	if err != nil {
		t.Fatalf("List batch history: %v", err)
	}
	if len(listed.Batches) != 1 || listed.Batches[0].BatchID != applied.BatchID {
		t.Errorf("batch history = %#v", listed)
	}
	shown, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show batch history: %v", err)
	}
	if len(shown.Entries) != 1 || shown.Entries[0].Source.Path != batchSource {
		t.Errorf("batch detail = %#v", shown)
	}
	if _, err := batchService.Undo(ctx, projectionbatch.UndoRequest{
		BatchID: applied.BatchID,
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		Caller:  projectionbatch.Caller{UID: 1000, GID: 1000},
	}); !errors.Is(err, namespace.ErrLibraryNotFound) {
		t.Errorf("Undo retired Library error = %v, want ErrLibraryNotFound", err)
	}

	if err := activeHandle.Close(); err != nil {
		t.Fatalf("Close active Overlay: %v", err)
	}
	if _, err := store.GarbageCollect(ctx); err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	assertStoredObject(t, opened, storePath, activeObjectID, false)
	assertLastOperation(t, opened, "library.remove", 6)
	assertVersionAndLogCount(t, opened, 6, 6)
}

func TestServiceRemoveRollsBackTreeAndRetirementWhenOperationLogFails(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	service := NewService(repository)
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")
	var child namespace.Node
	if _, err := repository.Update(
		ctx,
		namespace.Operation{
			Name:  "test.directory.create",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{"media/Movies"},
				NodeKinds: []namespace.NodeKind{namespace.Directory},
			},
		},
		func(transaction *namespace.Transaction) error {
			var err error
			child, err = transaction.CreateNode(ctx, namespace.CreateNode{
				ParentID: created.RootNodeID,
				Name:     "Movies",
				Kind:     namespace.Directory,
				Creator:  namespace.Actor{Kind: namespace.ActorSystem},
				Attributes: namespace.Attributes{
					Mode: 0o775,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create child: %v", err)
	}
	injectOperationFailure(t, opened, "library.remove")

	if _, err := service.Remove(ctx, RemoveRequest{
		Name:      "media",
		Recursive: true,
		Actor:     namespace.Actor{Kind: namespace.ActorSystem},
	}); err == nil {
		t.Fatal("Remove succeeded with forced operation-log failure")
	}
	found, err := service.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get after rollback: %v", err)
	}
	if found != created {
		t.Errorf("Library after rollback = %#v, want %#v", found, created)
	}
	if _, err := repository.Node(ctx, child.ID); err != nil {
		t.Errorf("read child after rollback: %v", err)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
}

func TestServiceRemoveRecursiveRejectsInFlightNamespaceWrite(t *testing.T) {
	opened := openTestDatabase(t)
	firstRepository := namespace.NewRepository(opened)
	secondRepository := namespace.NewRepository(opened)
	service := NewService(secondRepository)
	created := createTestLibrary(t, service, "media", "jellyfin")
	entered := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		_, err := firstRepository.Update(
			context.Background(),
			namespace.Operation{
				Name:  "test.write",
				Actor: namespace.Actor{Kind: namespace.ActorSystem},
				Details: namespace.OperationDetails{
					Paths:     []string{"media"},
					NodeKinds: []namespace.NodeKind{namespace.LibraryRoot},
				},
			},
			func(*namespace.Transaction) error {
				close(entered)
				<-release
				return nil
			},
		)
		writeDone <- err
	}()
	<-entered

	removeDone := make(chan error, 1)
	go func() {
		_, err := service.Remove(context.Background(), RemoveRequest{
			Name:      "media",
			Recursive: true,
			Actor:     namespace.Actor{Kind: namespace.ActorSystem},
		})
		removeDone <- err
	}()
	select {
	case err := <-removeDone:
		if !errors.Is(err, namespace.ErrWriteInProgress) {
			t.Fatalf("Remove error = %v, want ErrWriteInProgress", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove waited for in-flight namespace write")
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatalf("in-flight write: %v", err)
	}
	found, err := service.Get(context.Background(), "media")
	if err != nil {
		t.Fatalf("Get after rejected removal: %v", err)
	}
	if found != created {
		t.Errorf("Library after rejected removal = %#v, want %#v", found, created)
	}
	assertVersionAndLogCount(t, opened, 1, 1)
}

func TestServiceRemovePersistsRetirementAcrossRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	first, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	service := NewService(namespace.NewRepository(first))
	created := createTestLibrary(t, service, "media", "jellyfin")
	if _, err := service.Remove(ctx, RemoveRequest{
		Name:  "media",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer second.Close()
	if libraries, err := NewService(namespace.NewRepository(second)).List(ctx); err != nil {
		t.Fatalf("List after restart: %v", err)
	} else if len(libraries) != 0 {
		t.Errorf("Libraries after restart = %#v, want empty", libraries)
	}
	var status namespace.LibraryStatus
	var rootNodeID sql.NullInt64
	var retiredAt sql.NullInt64
	var rootCount int
	if err := second.UseConnection(ctx, func(connection *database.Connection) error {
		if err := connection.QueryRowContext(ctx, `
			SELECT status, root_node_id, retired_at
			FROM libraries
			WHERE id = ?
		`, created.ID).Scan(&status, &rootNodeID, &retiredAt); err != nil {
			return err
		}
		return connection.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM nodes WHERE id = ?",
			created.RootNodeID,
		).Scan(&rootCount)
	}); err != nil {
		t.Fatalf("read retirement after restart: %v", err)
	}
	if status != namespace.LibraryRetired || rootNodeID.Valid || !retiredAt.Valid || rootCount != 0 {
		t.Errorf(
			"retirement after restart = status %q root %#v retired_at %#v root count %d",
			status,
			rootNodeID,
			retiredAt,
			rootCount,
		)
	}
	assertVersionAndLogCount(t, second, 2, 2)
}

func testOverlayOperation(path string) namespace.Operation {
	return namespace.Operation{
		Name:  "test.overlay.create",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
		Details: namespace.OperationDetails{
			Paths:     []string{path},
			NodeKinds: []namespace.NodeKind{namespace.OverlayFile},
		},
	}
}

func testOverlayRequest(parentID int64, name string) namespace.CreateOverlay {
	return namespace.CreateOverlay{
		ParentID: parentID,
		Name:     name,
		Creator:  namespace.Actor{Kind: namespace.ActorSystem},
		Attributes: namespace.Attributes{
			Mode: 0o660,
		},
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("file %q content = %q, want %q", path, got, want)
	}
}

func assertStoredObject(
	t *testing.T,
	opened *database.DB,
	storePath string,
	objectID int64,
	want bool,
) {
	t.Helper()
	_, pathErr := os.Stat(filepath.Join(storePath, "live", strconv.FormatInt(objectID, 10)))
	if want && pathErr != nil {
		t.Errorf("object %d path: %v", objectID, pathErr)
	}
	if !want && !errors.Is(pathErr, os.ErrNotExist) {
		t.Errorf("object %d path error = %v, want not exist", objectID, pathErr)
	}
	var count int
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM objects WHERE id = ?",
			objectID,
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count object %d record: %v", objectID, err)
	}
	if got := count == 1; got != want {
		t.Errorf("object %d record exists = %v, want %v", objectID, got, want)
	}
}
