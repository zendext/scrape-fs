package namespace

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
)

func TestIndexRebuildsNamespaceAndObjectMappings(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	library := createIndexedLibrary(t, repository, "media")

	var objectID int64
	if err := opened.UseConnection(ctx, func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		now := time.Now().UTC().UnixNano()
		result, err := transaction.ExecContext(ctx, `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES ('STAGING', ?, NULL, ?)
		`, now, now)
		if err != nil {
			return err
		}
		objectID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("create object row: %v", err)
	}
	if _, err := repository.Update(ctx, testOperation("overlay.create"), func(transaction *Transaction) error {
		_, err := transaction.CreateOverlay(ctx, objectID, CreateOverlay{
			ParentID: library.RootNodeID,
			Name:     "poster.jpg",
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o640,
				UID:  1000,
				GID:  1000,
			},
		})
		return err
	}); err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}

	index := NewIndex(repository)
	if _, err := index.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	stats := index.Stats()
	if stats.Nodes != 3 || stats.Objects != 1 || stats.Libraries != 1 {
		t.Errorf("Index stats = %#v, want 3 nodes, 1 object, 1 Library", stats)
	}
}

func TestIndexInspectDetectsRepositoryDivergenceWithoutRebuild(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	createIndexedLibrary(t, repository, "media")

	index := NewIndex(repository)
	if _, err := index.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	createIndexedLibrary(t, repository, "archive")

	inspection, err := index.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !inspection.Diverged {
		t.Fatal("Inspect Diverged = false, want true")
	}
	if inspection.Index != (IndexStats{Libraries: 1, Nodes: 2}) {
		t.Errorf("indexed stats = %#v", inspection.Index)
	}
	if inspection.Repository != (IndexStats{Libraries: 2, Nodes: 3}) {
		t.Errorf("Repository stats = %#v", inspection.Repository)
	}
	if got := index.Stats(); got != inspection.Index {
		t.Errorf("Inspect mutated index stats to %#v, want %#v", got, inspection.Index)
	}
}

func TestIndexInspectDoesNotReportCommitListenerRebuildWindow(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	createIndexedLibrary(t, repository, "media")

	index := NewIndex(repository)
	if _, err := index.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	listenerEntered := make(chan struct{})
	releaseListener := make(chan struct{})
	repository.AddCommitListener(func(Commit) {
		close(listenerEntered)
		<-releaseListener
		if _, err := index.Rebuild(context.Background()); err != nil {
			t.Errorf("listener Rebuild: %v", err)
		}
	})
	writeDone := make(chan error, 1)
	go func() {
		_, err := repository.Update(
			context.Background(),
			testOperation("library.create"),
			func(transaction *Transaction) error {
				_, err := transaction.CreateLibrary(
					context.Background(),
					CreateLibrary{
						Name:           "archive",
						ProfileName:    "jellyfin",
						ProfileVersion: 1,
						Creator:        Actor{Kind: ActorSystem},
						UID:            1000,
						GID:            1000,
						RootMode:       0o775,
						DirectoryMode:  0o775,
						ProjectionMode: 0o444,
						OverlayMode:    0o664,
					},
				)
				return err
			},
		)
		writeDone <- err
	}()
	<-listenerEntered

	inspectDone := make(chan IndexInspection, 1)
	go func() {
		inspection, err := index.Inspect(ctx)
		if err != nil {
			t.Errorf("Inspect: %v", err)
		}
		inspectDone <- inspection
	}()
	time.Sleep(time.Millisecond)
	close(releaseListener)

	inspection := <-inspectDone
	if err := <-writeDone; err != nil {
		t.Fatalf("create archive Library: %v", err)
	}
	if inspection.Diverged {
		t.Errorf("Inspect reported listener rebuild window as divergence: %#v", inspection)
	}
}

func TestRepositoryInspectValidatesLibraryRootRelationships(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	library := createIndexedLibrary(t, repository, "media")

	inspection, err := repository.Inspect(ctx)
	if err != nil {
		t.Fatalf("healthy Inspect: %v", err)
	}
	if inspection.NamespaceVersion != 1 ||
		inspection.ActiveLibraries != 1 ||
		inspection.RetiredLibraries != 0 ||
		inspection.Nodes != 2 ||
		inspection.Projections != 0 ||
		inspection.OverlayFiles != 0 {
		t.Errorf("healthy inspection = %#v", inspection)
	}

	if err := opened.UseConnection(ctx, func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(
			ctx,
			"UPDATE nodes SET name = 'mismatched' WHERE id = ?",
			library.RootNodeID,
		); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("corrupt LibraryRoot relationship: %v", err)
	}

	if _, err := repository.Inspect(ctx); !errors.Is(err, ErrInvalidLibrary) {
		t.Fatalf("corrupt Inspect error = %v, want ErrInvalidLibrary", err)
	}
}

func TestRepositoryCommitListenerRebuildsDeterministicIndexDiff(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	library := createIndexedLibrary(t, repository, "media")
	index := NewIndex(repository)
	if _, err := index.Rebuild(ctx); err != nil {
		t.Fatalf("initial Rebuild: %v", err)
	}

	var gotCommit Commit
	var gotChanges ChangeSet
	var rebuildErr error
	repository.AddCommitListener(func(commit Commit) {
		gotCommit = commit
		gotChanges, rebuildErr = index.Rebuild(context.Background())
	})
	actorID := int64(1000)
	if _, err := repository.Update(ctx, Operation{
		Name:  "library.rename",
		Actor: Actor{Kind: ActorControl, ID: &actorID},
		Details: OperationDetails{
			Paths:     []string{"media", "movies"},
			NodeKinds: []NodeKind{LibraryRoot, LibraryRoot},
		},
	}, func(transaction *Transaction) error {
		_, err := transaction.RenameLibrary(ctx, "media", "movies")
		return err
	}); err != nil {
		t.Fatalf("RenameLibrary: %v", err)
	}
	if rebuildErr != nil {
		t.Fatalf("listener Rebuild: %v", rebuildErr)
	}
	if gotCommit.Operation != "library.rename" ||
		gotCommit.Actor.Kind != ActorControl ||
		gotCommit.Actor.ID == nil ||
		*gotCommit.Actor.ID != actorID {
		t.Errorf("Commit = %#v", gotCommit)
	}
	wantEntries := []EntryChange{
		{ParentID: 1, NodeID: library.RootNodeID, Name: "media", Removed: true},
		{ParentID: 1, NodeID: library.RootNodeID, Name: "movies"},
	}
	if !equalEntryChanges(gotChanges.Entries, wantEntries) {
		t.Errorf("Entry changes = %#v, want %#v", gotChanges.Entries, wantEntries)
	}
	if !containsInt64(gotChanges.InodeIDs, 1) ||
		!containsInt64(gotChanges.InodeIDs, library.RootNodeID) {
		t.Errorf("Inode changes = %v, want GlobalRoot and LibraryRoot", gotChanges.InodeIDs)
	}
}

func TestSetAttributesNotifiesAfterReleasingDatabaseConnection(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	library := createIndexedLibrary(t, repository, "media")
	var directory Node
	if _, err := repository.Update(
		ctx,
		testOperation("directory.create"),
		func(transaction *Transaction) error {
			var err error
			directory, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: library.RootNodeID,
				Name:     "Movies",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
					UID:  1000,
					GID:  1000,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	index := NewIndex(repository)
	if _, err := index.Rebuild(ctx); err != nil {
		t.Fatalf("initial Rebuild: %v", err)
	}
	var gotCommit Commit
	var gotChanges ChangeSet
	var rebuildErr error
	repository.AddCommitListener(func(commit Commit) {
		gotCommit = commit
		rebuildCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		gotChanges, rebuildErr = index.Rebuild(rebuildCtx)
	})

	mode := fs.FileMode(0o750)
	if _, err := repository.SetAttributes(ctx, SetAttributes{
		NodeID: directory.ID,
		Mode:   &mode,
	}); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	if rebuildErr != nil {
		t.Fatalf("listener Rebuild: %v", rebuildErr)
	}
	if gotCommit.Operation != "namespace.set-attributes" {
		t.Errorf("Commit = %#v, want namespace.set-attributes", gotCommit)
	}
	if !containsInt64(gotChanges.InodeIDs, directory.ID) {
		t.Errorf("Inode changes = %v, want %d", gotChanges.InodeIDs, directory.ID)
	}
}

func TestSetAttributesListenerCompletesBeforeNextWrite(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	library := createIndexedLibrary(t, repository, "media")
	var directory Node
	if _, err := repository.Update(
		ctx,
		testOperation("directory.create"),
		func(transaction *Transaction) error {
			var err error
			directory, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: library.RootNodeID,
				Name:     "Movies",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
					UID:  1000,
					GID:  1000,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	listenerEntered := make(chan struct{})
	releaseListener := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	repository.AddCommitListener(func(Commit) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(listenerEntered)
			<-releaseListener
		}
	})

	mode := fs.FileMode(0o750)
	attributesDone := make(chan error, 1)
	go func() {
		_, err := repository.SetAttributes(ctx, SetAttributes{
			NodeID: directory.ID,
			Mode:   &mode,
		})
		attributesDone <- err
	}()
	select {
	case <-listenerEntered:
	case <-time.After(time.Second):
		t.Fatal("SetAttributes listener did not start")
	}

	secondWriteDone := make(chan error, 1)
	go func() {
		_, err := repository.Update(
			ctx,
			testOperation("test.second"),
			func(*Transaction) error {
				return nil
			},
		)
		secondWriteDone <- err
	}()
	select {
	case err := <-secondWriteDone:
		t.Fatalf("second write completed before listener: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseListener)
	if err := <-attributesDone; err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	if err := <-secondWriteDone; err != nil {
		t.Fatalf("second Update: %v", err)
	}
}

func createIndexedLibrary(
	t *testing.T,
	repository *Repository,
	name string,
) Library {
	t.Helper()
	var library Library
	if _, err := repository.Update(context.Background(), testOperation("library.create"), func(transaction *Transaction) error {
		var err error
		library, err = transaction.CreateLibrary(context.Background(), CreateLibrary{
			Name:           name,
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
	}); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	return library
}

func equalEntryChanges(got, want []EntryChange) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func containsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
