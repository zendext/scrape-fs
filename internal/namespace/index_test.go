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

	type inspectResult struct {
		inspection IndexInspection
		err        error
	}
	inspectDone := make(chan inspectResult, 1)
	go func() {
		inspection, err := index.Inspect(ctx)
		inspectDone <- inspectResult{inspection: inspection, err: err}
	}()

	select {
	case result := <-inspectDone:
		close(releaseListener)
		<-writeDone
		t.Fatalf(
			"Inspect completed during an active commit listener after %v: %#v/%v",
			20*time.Millisecond,
			result.inspection,
			result.err,
		)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseListener)

	result := <-inspectDone
	if result.err != nil {
		t.Fatalf("Inspect: %v", result.err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("create archive Library: %v", err)
	}
	if result.inspection.Diverged {
		t.Errorf(
			"Inspect reported listener rebuild window as divergence: %#v",
			result.inspection,
		)
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

func TestRepositoryInspectRejectsChildrenBelowFileNodesWithoutMutation(t *testing.T) {
	for _, kind := range []NodeKind{ProjectionFile, OverlayFile} {
		t.Run(string(kind), func(t *testing.T) {
			opened := openTestDatabase(t)
			repository := NewRepository(opened)
			ctx := context.Background()
			library := createIndexedLibrary(t, repository, "media")
			parent := createIndexedFileNode(t, opened, repository, library, kind)

			var child Node
			if _, err := repository.Update(
				ctx,
				testOperation("directory.create"),
				func(transaction *Transaction) error {
					var err error
					child, err = transaction.CreateNode(ctx, CreateNode{
						ParentID: library.RootNodeID,
						Name:     "nested",
						Kind:     Directory,
						Creator:  Actor{Kind: ActorSystem},
						Attributes: Attributes{
							Mode: 0o755,
							UID:  1000,
							GID:  1000,
						},
					})
					return err
				},
			); err != nil {
				t.Fatalf("create child Directory: %v", err)
			}
			if err := opened.UseConnection(ctx, func(connection *database.Connection) error {
				transaction, err := connection.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer transaction.Rollback()
				if _, err := transaction.ExecContext(
					ctx,
					"UPDATE nodes SET parent_id = ? WHERE id = ?",
					parent.ID,
					child.ID,
				); err != nil {
					return err
				}
				return transaction.Commit()
			}); err != nil {
				t.Fatalf("corrupt child parent: %v", err)
			}

			versionBefore, err := repository.NamespaceVersion(ctx)
			if err != nil {
				t.Fatalf("NamespaceVersion before Inspect: %v", err)
			}
			if _, err := repository.Inspect(ctx); !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Inspect error = %v, want ErrInvalidNode", err)
			}
			versionAfter, err := repository.NamespaceVersion(ctx)
			if err != nil {
				t.Fatalf("NamespaceVersion after Inspect: %v", err)
			}
			if versionAfter != versionBefore {
				t.Errorf(
					"Inspect changed namespace version from %d to %d",
					versionBefore,
					versionAfter,
				)
			}
			got, err := repository.Node(ctx, child.ID)
			if err != nil {
				t.Fatalf("read corrupt child after Inspect: %v", err)
			}
			if got.ParentID == nil || *got.ParentID != parent.ID {
				t.Errorf("Inspect changed corrupt child parent: %#v", got.ParentID)
			}
		})
	}
}

func TestRepositoryInspectRejectsUnassociatedNestedLibraryRootWithoutMutation(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	media := createIndexedLibrary(t, repository, "media")
	nested := createIndexedLibrary(t, repository, "nested")

	if err := opened.UseConnection(ctx, func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		now := time.Now().UTC().UnixNano()
		if _, err := transaction.ExecContext(ctx, `
			UPDATE libraries
			SET status = 'RETIRED', root_node_id = NULL, updated_at = ?, retired_at = ?
			WHERE id = ?
		`, now, now, nested.ID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(
			ctx,
			"UPDATE nodes SET parent_id = ? WHERE id = ?",
			media.RootNodeID,
			nested.RootNodeID,
		); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("corrupt nested LibraryRoot: %v", err)
	}

	versionBefore, err := repository.NamespaceVersion(ctx)
	if err != nil {
		t.Fatalf("NamespaceVersion before Inspect: %v", err)
	}
	if _, err := repository.Inspect(ctx); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Inspect error = %v, want ErrInvalidNode", err)
	}
	versionAfter, err := repository.NamespaceVersion(ctx)
	if err != nil {
		t.Fatalf("NamespaceVersion after Inspect: %v", err)
	}
	if versionAfter != versionBefore {
		t.Errorf(
			"Inspect changed namespace version from %d to %d",
			versionBefore,
			versionAfter,
		)
	}
	got, err := repository.Node(ctx, nested.RootNodeID)
	if err != nil {
		t.Fatalf("read nested LibraryRoot after Inspect: %v", err)
	}
	if got.ParentID == nil || *got.ParentID != media.RootNodeID {
		t.Errorf("Inspect changed nested LibraryRoot parent: %#v", got.ParentID)
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

func createIndexedFileNode(
	t *testing.T,
	opened *database.DB,
	repository *Repository,
	library Library,
	kind NodeKind,
) Node {
	t.Helper()

	var objectID int64
	if kind == OverlayFile {
		if err := opened.UseConnection(
			context.Background(),
			func(connection *database.Connection) error {
				transaction, err := connection.BeginTx(context.Background(), nil)
				if err != nil {
					return err
				}
				defer transaction.Rollback()
				now := time.Now().UTC().UnixNano()
				result, err := transaction.ExecContext(context.Background(), `
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
			},
		); err != nil {
			t.Fatalf("create object row: %v", err)
		}
	}

	var node Node
	if _, err := repository.Update(
		context.Background(),
		testOperation("file.create"),
		func(transaction *Transaction) error {
			var err error
			switch kind {
			case ProjectionFile:
				node, err = transaction.CreateNode(context.Background(), CreateNode{
					ParentID: library.RootNodeID,
					Name:     "parent.projection",
					Kind:     ProjectionFile,
					Source: &ProjectionSource{
						Path:     "/source/parent.projection",
						DeviceID: 1,
						Inode:    2,
						Size:     3,
					},
					Creator: Actor{Kind: ActorSystem},
					Attributes: Attributes{
						Mode: 0o444,
						UID:  1000,
						GID:  1000,
					},
				})
			case OverlayFile:
				node, err = transaction.CreateOverlay(
					context.Background(),
					objectID,
					CreateOverlay{
						ParentID: library.RootNodeID,
						Name:     "parent.overlay",
						Creator:  Actor{Kind: ActorSystem},
						Attributes: Attributes{
							Mode: 0o644,
							UID:  1000,
							GID:  1000,
						},
					},
				)
			default:
				t.Fatalf("unsupported file kind %q", kind)
			}
			return err
		},
	); err != nil {
		t.Fatalf("create %s: %v", kind, err)
	}
	return node
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
