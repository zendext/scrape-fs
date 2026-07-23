package namespace

import (
	"context"
	"errors"
	"testing"
	"time"

	databasepkg "github.com/zendext/scrape-fs/internal/database"
)

func TestTransactionCreateOverlayPromotesObjectAndCreatesNodeAtomically(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	repository.now = func() time.Time {
		return time.Unix(1_700_000_000, 123)
	}
	ctx := context.Background()

	libraryRootID := createTestLibraryRoot(t, repository)
	objectID := insertStagingObject(t, opened, 10)
	actorID := int64(1000)
	var created Node
	version, err := repository.Update(
		ctx,
		Operation{
			Name:  "overlay.create",
			Actor: Actor{Kind: ActorFuse, ID: &actorID},
			Details: OperationDetails{
				Paths:     []string{"library/poster.jpg"},
				NodeKinds: []NodeKind{OverlayFile},
			},
		},
		func(transaction *Transaction) error {
			var err error
			created, err = transaction.CreateOverlay(ctx, objectID, CreateOverlay{
				ParentID: libraryRootID,
				Name:     "poster.jpg",
				Creator:  Actor{Kind: ActorFuse, ID: &actorID},
				Attributes: Attributes{
					Mode: 0o664,
					UID:  1000,
					GID:  1001,
				},
			})
			return err
		},
	)
	if err != nil {
		t.Fatalf("CreateOverlay: %v", err)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}
	if created.Kind != OverlayFile ||
		created.ObjectID == nil ||
		*created.ObjectID != objectID {
		t.Errorf("created node = %#v, want OverlayFile for object %d", created, objectID)
	}

	state, createdAt, liveAt, updatedAt := readObjectLifecycle(t, opened, objectID)
	wantLiveAt := repository.now().UTC().UnixNano()
	if state != "LIVE" || createdAt != 10 || liveAt == nil || *liveAt != wantLiveAt || updatedAt != wantLiveAt {
		t.Errorf(
			"object lifecycle = %q/%d/%v/%d, want LIVE/10/%d/%d",
			state,
			createdAt,
			liveAt,
			updatedAt,
			wantLiveAt,
			wantLiveAt,
		)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
}

func TestTransactionCreateOverlayRollsBackPromotionOnNodeConflict(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()

	libraryRootID := createTestLibraryRoot(t, repository)
	firstObjectID := insertStagingObject(t, opened, 10)
	secondObjectID := insertStagingObject(t, opened, 20)
	create := func(objectID int64) error {
		_, err := repository.Update(
			ctx,
			Operation{
				Name:  "overlay.create",
				Actor: Actor{Kind: ActorSystem},
				Details: OperationDetails{
					Paths:     []string{"library/poster.jpg"},
					NodeKinds: []NodeKind{OverlayFile},
				},
			},
			func(transaction *Transaction) error {
				_, err := transaction.CreateOverlay(ctx, objectID, CreateOverlay{
					ParentID: libraryRootID,
					Name:     "poster.jpg",
					Creator:  Actor{Kind: ActorSystem},
					Attributes: Attributes{
						Mode: 0o600,
					},
				})
				return err
			},
		)
		return err
	}
	if err := create(firstObjectID); err != nil {
		t.Fatalf("create first Overlay: %v", err)
	}
	if err := create(secondObjectID); !errors.Is(err, ErrConflict) {
		t.Fatalf("create conflicting Overlay error = %v, want ErrConflict", err)
	}

	state, _, liveAt, updatedAt := readObjectLifecycle(t, opened, secondObjectID)
	if state != "STAGING" || liveAt != nil || updatedAt != 20 {
		t.Errorf(
			"rolled-back object lifecycle = %q/%v/%d, want STAGING/NULL/20",
			state,
			liveAt,
			updatedAt,
		)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
}

func TestTransactionCreateNodeRejectsStagingOverlayObject(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()

	libraryRootID := createTestLibraryRoot(t, repository)
	objectID := insertStagingObject(t, opened, 10)
	_, err := repository.Update(
		ctx,
		testOperation("overlay.create"),
		func(transaction *Transaction) error {
			_, err := transaction.CreateNode(ctx, CreateNode{
				ParentID: libraryRootID,
				Name:     "poster.jpg",
				Kind:     OverlayFile,
				ObjectID: &objectID,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o600,
				},
			})
			return err
		},
	)
	if !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("CreateNode error = %v, want ErrInvalidObject", err)
	}
	assertVersionAndLogCount(t, opened, 1, 1)
}

func createTestLibraryRoot(t *testing.T, repository *Repository) int64 {
	t.Helper()
	ctx := context.Background()
	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}
	var libraryRootID int64
	if _, err := repository.Update(ctx, testOperation("library.create"), func(transaction *Transaction) error {
		node, err := transaction.CreateNode(ctx, CreateNode{
			ParentID: root.ID,
			Name:     "library",
			Kind:     LibraryRoot,
			Creator:  Actor{Kind: ActorSystem},
			Attributes: Attributes{
				Mode: 0o775,
			},
		})
		libraryRootID = node.ID
		return err
	}); err != nil {
		t.Fatalf("create LibraryRoot: %v", err)
	}
	return libraryRootID
}

func insertStagingObject(t *testing.T, opened *databasepkg.DB, now int64) int64 {
	t.Helper()
	var objectID int64
	if err := opened.UseConnection(context.Background(), func(connection *databasepkg.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
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
	}); err != nil {
		t.Fatalf("insert STAGING object: %v", err)
	}
	return objectID
}

func readObjectLifecycle(
	t *testing.T,
	opened *databasepkg.DB,
	objectID int64,
) (string, int64, *int64, int64) {
	t.Helper()
	var state string
	var createdAt int64
	var liveAt *int64
	var updatedAt int64
	if err := opened.UseConnection(context.Background(), func(connection *databasepkg.Connection) error {
		return connection.QueryRowContext(context.Background(), `
			SELECT state, created_at_ns, live_at_ns, updated_at_ns
			FROM objects
			WHERE id = ?
		`, objectID).Scan(&state, &createdAt, &liveAt, &updatedAt)
	}); err != nil {
		t.Fatalf("read object lifecycle: %v", err)
	}
	return state, createdAt, liveAt, updatedAt
}
