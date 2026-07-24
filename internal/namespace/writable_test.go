package namespace

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"
)

func TestTransactionRemoveDirectoryRequiresEmptyDirectory(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	now := time.Unix(1_700_000_000, 1)
	repository.now = func() time.Time {
		return now
	}
	ctx := context.Background()
	rootID := createTestLibraryRoot(t, repository)

	var parent Node
	if _, err := repository.Update(
		ctx,
		testOperation("directory.create"),
		func(transaction *Transaction) error {
			var err error
			parent, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: rootID,
				Name:     "parent",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
				},
			})
			if err != nil {
				return err
			}
			_, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: parent.ID,
				Name:     "child",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create directory tree: %v", err)
	}
	rootBeforeFailure, err := repository.Node(ctx, rootID)
	if err != nil {
		t.Fatalf("read root before failed removal: %v", err)
	}

	if _, err := repository.Update(
		ctx,
		testOperation("directory.remove"),
		func(transaction *Transaction) error {
			_, err := transaction.RemoveDirectory(ctx, rootID, "parent")
			return err
		},
	); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("RemoveDirectory non-empty error = %v, want ErrNotEmpty", err)
	}
	rootAfterFailure, err := repository.Node(ctx, rootID)
	if err != nil {
		t.Fatalf("read root after failed removal: %v", err)
	}
	if rootAfterFailure.MtimeNS != rootBeforeFailure.MtimeNS ||
		rootAfterFailure.VirtualCtimeNS != rootBeforeFailure.VirtualCtimeNS {
		t.Errorf(
			"failed removal changed root timestamps from %d/%d to %d/%d",
			rootBeforeFailure.MtimeNS,
			rootBeforeFailure.VirtualCtimeNS,
			rootAfterFailure.MtimeNS,
			rootAfterFailure.VirtualCtimeNS,
		)
	}
	assertVersionAndLogCount(t, opened, 2, 2)

	now = now.Add(time.Second)
	if _, err := repository.Update(
		ctx,
		Operation{
			Name:  "directory.remove",
			Actor: Actor{Kind: ActorSystem},
			Details: OperationDetails{
				Paths:     []string{"library/parent/child", "library/parent"},
				NodeKinds: []NodeKind{Directory, Directory},
			},
		},
		func(transaction *Transaction) error {
			if _, err := transaction.RemoveDirectory(ctx, parent.ID, "child"); err != nil {
				return err
			}
			_, err := transaction.RemoveDirectory(ctx, rootID, "parent")
			return err
		},
	); err != nil {
		t.Fatalf("remove empty directory tree: %v", err)
	}
	rootAfterSuccess, err := repository.Node(ctx, rootID)
	if err != nil {
		t.Fatalf("read root after successful removal: %v", err)
	}
	if rootAfterSuccess.MtimeNS != now.UTC().UnixNano() ||
		rootAfterSuccess.VirtualCtimeNS != now.UTC().UnixNano() {
		t.Errorf(
			"successful removal root timestamps = %d/%d, want %d",
			rootAfterSuccess.MtimeNS,
			rootAfterSuccess.VirtualCtimeNS,
			now.UTC().UnixNano(),
		)
	}
	assertVersionAndLogCount(t, opened, 3, 3)
}

func TestTransactionRemoveOverlayReturnsBackingObject(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	rootID := createTestLibraryRoot(t, repository)
	objectID := insertStagingObject(t, opened, 10)

	if _, err := repository.Update(
		ctx,
		testOperation("overlay.create"),
		func(transaction *Transaction) error {
			_, err := transaction.CreateOverlay(ctx, objectID, CreateOverlay{
				ParentID: rootID,
				Name:     "poster.jpg",
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o664,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create Overlay: %v", err)
	}

	var removed Node
	if _, err := repository.Update(
		ctx,
		testOperation("overlay.unlink"),
		func(transaction *Transaction) error {
			var err error
			removed, err = transaction.RemoveOverlay(ctx, rootID, "poster.jpg")
			return err
		},
	); err != nil {
		t.Fatalf("RemoveOverlay: %v", err)
	}
	if removed.Kind != OverlayFile ||
		removed.ObjectID == nil ||
		*removed.ObjectID != objectID {
		t.Errorf("removed node = %#v, want Overlay object %d", removed, objectID)
	}
	if _, err := repository.Child(ctx, rootID, "poster.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Child after RemoveOverlay error = %v, want ErrNotFound", err)
	}
	assertVersionAndLogCount(t, opened, 3, 3)
}

func TestTransactionRenameAppliesDocumentedMatrix(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	firstRootID := createTestLibraryRoot(t, repository)
	secondRootID := createTestLibraryRootNamed(t, repository, "second")
	sourceObjectID := insertStagingObject(t, opened, 10)
	targetObjectID := insertStagingObject(t, opened, 20)

	if _, err := repository.Update(
		ctx,
		Operation{
			Name:  "overlay.create",
			Actor: Actor{Kind: ActorSystem},
			Details: OperationDetails{
				Paths:     []string{"library/source", "library/target"},
				NodeKinds: []NodeKind{OverlayFile, OverlayFile},
			},
		},
		func(transaction *Transaction) error {
			if _, err := transaction.CreateOverlay(ctx, sourceObjectID, CreateOverlay{
				ParentID: firstRootID,
				Name:     "source",
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o664,
				},
			}); err != nil {
				return err
			}
			_, err := transaction.CreateOverlay(ctx, targetObjectID, CreateOverlay{
				ParentID: firstRootID,
				Name:     "target",
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o664,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create Overlays: %v", err)
	}

	var result RenameResult
	if _, err := repository.Update(
		ctx,
		Operation{
			Name:  "node.rename",
			Actor: Actor{Kind: ActorSystem},
			Details: OperationDetails{
				Paths:     []string{"library/source", "library/target"},
				NodeKinds: []NodeKind{OverlayFile, OverlayFile},
			},
		},
		func(transaction *Transaction) error {
			var err error
			result, err = transaction.Rename(ctx, RenameNode{
				OldParentID: firstRootID,
				OldName:     "source",
				NewParentID: firstRootID,
				NewName:     "target",
			})
			return err
		},
	); err != nil {
		t.Fatalf("Rename Overlay over Overlay: %v", err)
	}
	if result.Node.Name != "target" ||
		result.Node.ObjectID == nil ||
		*result.Node.ObjectID != sourceObjectID ||
		result.Replaced == nil ||
		result.Replaced.ObjectID == nil ||
		*result.Replaced.ObjectID != targetObjectID {
		t.Errorf("Rename result = %#v", result)
	}

	if _, err := repository.Update(
		ctx,
		testOperation("node.rename"),
		func(transaction *Transaction) error {
			_, err := transaction.Rename(ctx, RenameNode{
				OldParentID: firstRootID,
				OldName:     "target",
				NewParentID: secondRootID,
				NewName:     "target",
			})
			return err
		},
	); !errors.Is(err, ErrCrossLibrary) {
		t.Fatalf("cross-Library Rename error = %v, want ErrCrossLibrary", err)
	}
}

func TestTransactionRenameRejectsDirectoryDescendantMove(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()
	rootID := createTestLibraryRoot(t, repository)

	var parent, child Node
	if _, err := repository.Update(
		ctx,
		testOperation("directory.create"),
		func(transaction *Transaction) error {
			var err error
			parent, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: rootID,
				Name:     "parent",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
				},
			})
			if err != nil {
				return err
			}
			child, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: parent.ID,
				Name:     "child",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create directory tree: %v", err)
	}

	if _, err := repository.Update(
		ctx,
		testOperation("directory.rename"),
		func(transaction *Transaction) error {
			_, err := transaction.Rename(ctx, RenameNode{
				OldParentID: rootID,
				OldName:     parent.Name,
				NewParentID: child.ID,
				NewName:     parent.Name,
			})
			return err
		},
	); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("descendant Rename error = %v, want ErrInvalidOperation", err)
	}
}

func TestRepositorySetAttributesDoesNotChangeNamespaceVersion(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	repository.now = func() time.Time {
		return time.Unix(1_700_000_000, 123)
	}
	ctx := context.Background()
	rootID := createTestLibraryRoot(t, repository)

	var directory Node
	if _, err := repository.Update(
		ctx,
		testOperation("directory.create"),
		func(transaction *Transaction) error {
			var err error
			directory, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: rootID,
				Name:     "Movies",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
					UID:  1000,
					GID:  1001,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create Directory: %v", err)
	}
	assertVersionAndLogCount(t, opened, 2, 2)

	mode := fs.FileMode(0o750)
	uid := uint32(2000)
	gid := uint32(2001)
	atime := int64(10)
	mtime := int64(20)
	updated, err := repository.SetAttributes(ctx, SetAttributes{
		NodeID:  directory.ID,
		Mode:    &mode,
		UID:     &uid,
		GID:     &gid,
		AtimeNS: &atime,
		MtimeNS: &mtime,
	})
	if err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	if updated.Attributes != (Attributes{Mode: mode, UID: uid, GID: gid}) ||
		updated.AtimeNS != atime ||
		updated.MtimeNS != mtime ||
		updated.VirtualCtimeNS != repository.now().UTC().UnixNano() {
		t.Errorf("updated Directory = %#v", updated)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
}

func createTestLibraryRootNamed(
	t *testing.T,
	repository *Repository,
	name string,
) int64 {
	t.Helper()
	ctx := context.Background()
	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}
	var rootID int64
	if _, err := repository.Update(
		ctx,
		testOperation("library.create"),
		func(transaction *Transaction) error {
			node, err := transaction.CreateNode(ctx, CreateNode{
				ParentID: root.ID,
				Name:     name,
				Kind:     LibraryRoot,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
				},
			})
			rootID = node.ID
			return err
		},
	); err != nil {
		t.Fatalf("create LibraryRoot %q: %v", name, err)
	}
	return rootID
}
