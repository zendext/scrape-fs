package projection_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestMoveBatchOwnedProjectionPreservesProvenanceAndUndo(t *testing.T) {
	ctx := context.Background()
	opened := openBatchOwnershipDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createBatchOwnershipLibrary(t, repository)
	sourcePath := filepath.Join(t.TempDir(), "Dune.mkv")
	sourceContent := []byte("batch source")
	if err := os.WriteFile(sourcePath, sourceContent, 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	batchService := projectionbatch.NewService(repository)
	data := batchOwnershipJSON(t, sourcePath, "Managed/Leaf/Dune.mkv")
	validation, err := batchService.Validate(ctx, data)
	if err != nil {
		t.Fatalf("Validate Batch: %v", err)
	}
	applied, err := batchService.Apply(ctx, projectionbatch.ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         projectionbatch.Caller{UID: 1000, GID: 1001},
	})
	if err != nil {
		t.Fatalf("Apply Batch: %v", err)
	}
	before, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed/Leaf/Dune.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup Batch Projection: %v", err)
	}
	if before.OriginScrapeID == nil {
		t.Fatal("Batch Projection OriginScrapeID = nil")
	}
	beforeDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch before move: %v", err)
	}
	beforeEntries, err := repository.ProjectionBatchEntries(ctx, *before.OriginScrapeID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries before move: %v", err)
	}

	projectionService := projection.NewService(repository)
	if _, err := projectionService.Move(ctx, projection.MoveRequest{
		Target:    "media:/Managed/Leaf/Dune.mkv",
		NewTarget: "media:/Moved/New/Dune.mkv",
		Actor:     namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Move Batch-owned Projection: %v", err)
	}
	moved, err := projectionService.Move(ctx, projection.MoveRequest{
		Target:    "media:/Moved/New/Dune.mkv",
		NewTarget: "media:/Final/New/Renamed.mkv",
		Actor:     namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Move Batch-owned Projection again: %v", err)
	}
	if moved.Node.ID != before.ID ||
		moved.Node.OriginScrapeID == nil ||
		*moved.Node.OriginScrapeID != *before.OriginScrapeID ||
		moved.Node.Creator != before.Creator ||
		moved.Node.Attributes != before.Attributes ||
		moved.Node.Source == nil ||
		before.Source == nil ||
		*moved.Node.Source != *before.Source {
		t.Errorf("moved Batch Projection = %#v, want provenance from %#v", moved.Node, before)
	}
	if _, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed/Leaf/Dune.mkv",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("old target error = %v, want ErrNotFound", err)
	}
	if current, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Final/New/Renamed.mkv",
	); err != nil || current.ID != before.ID {
		t.Errorf("moved target = %#v/%v, want node %d", current, err, before.ID)
	}
	afterEntries, err := repository.ProjectionBatchEntries(ctx, *before.OriginScrapeID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries after move: %v", err)
	}
	if !reflect.DeepEqual(afterEntries, beforeEntries) {
		t.Errorf("Batch entries changed: before=%#v after=%#v", beforeEntries, afterEntries)
	}
	afterDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch after move: %v", err)
	}
	if afterDetail.Batch != beforeDetail.Batch ||
		len(afterDetail.Entries) != 1 ||
		afterDetail.Entries[0].Target != "media:/Managed/Leaf/Dune.mkv" ||
		len(afterDetail.Operations) != 3 ||
		afterDetail.Operations[1].Name != "projection.move" ||
		!reflect.DeepEqual(
			afterDetail.Operations[1].Details.Paths,
			[]string{
				"media:/Managed/Leaf/Dune.mkv",
				"media:/Moved/New/Dune.mkv",
			},
		) ||
		afterDetail.Operations[2].Name != "projection.move" ||
		!reflect.DeepEqual(
			afterDetail.Operations[2].Details.Paths,
			[]string{
				"media:/Moved/New/Dune.mkv",
				"media:/Final/New/Renamed.mkv",
			},
		) {
		t.Errorf("Batch detail after move = %#v", afterDetail)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != string(sourceContent) {
		t.Errorf("source after move = %q/%v", got, err)
	}

	undone, err := batchService.Undo(ctx, projectionbatch.UndoRequest{
		BatchID: applied.BatchID,
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		Caller:  projectionbatch.Caller{UID: 1000, GID: 1001},
	})
	if err != nil {
		t.Fatalf("Undo Batch after move: %v", err)
	}
	if undone.Status != string(namespace.ProjectionBatchUndone) {
		t.Errorf("Undo status = %q, want undone", undone.Status)
	}
	if _, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Final/New/Renamed.mkv",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("moved Projection after Undo error = %v, want ErrNotFound", err)
	}
	for _, path := range []string{"Managed", "Moved", "Final"} {
		if _, err := repository.Lookup(
			ctx,
			libraryRecord.RootNodeID,
			path,
		); !errors.Is(err, namespace.ErrNotFound) {
			t.Errorf(
				"managed directory %q after Undo error = %v, want ErrNotFound",
				path,
				err,
			)
		}
	}
	undoneDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch after Undo: %v", err)
	}
	if undoneDetail.Batch.Status != string(namespace.ProjectionBatchUndone) ||
		len(undoneDetail.Entries) != 1 ||
		undoneDetail.Entries[0].ProjectionNodeID != nil ||
		len(undoneDetail.Operations) != 4 ||
		undoneDetail.Operations[1].Name != "projection.move" ||
		undoneDetail.Operations[2].Name != "projection.move" ||
		undoneDetail.Operations[3].Name != "projection.batch.undo" {
		t.Errorf("Batch detail after Undo = %#v", undoneDetail)
	}
}

func TestRemoveBatchOwnedProjectionPreservesBatchHistoryAndUndo(t *testing.T) {
	ctx := context.Background()
	opened := openBatchOwnershipDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createBatchOwnershipLibrary(t, repository)
	sourcePath := filepath.Join(t.TempDir(), "Dune.mkv")
	sourceContent := []byte("batch source")
	if err := os.WriteFile(sourcePath, sourceContent, 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	batchService := projectionbatch.NewService(repository)
	data := batchOwnershipJSON(t, sourcePath, "Managed/Leaf/Dune.mkv")
	validation, err := batchService.Validate(ctx, data)
	if err != nil {
		t.Fatalf("Validate Batch: %v", err)
	}
	applied, err := batchService.Apply(ctx, projectionbatch.ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         projectionbatch.Caller{UID: 1000, GID: 1001},
	})
	if err != nil {
		t.Fatalf("Apply Batch: %v", err)
	}
	before, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed/Leaf/Dune.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup Batch Projection: %v", err)
	}
	if before.OriginScrapeID == nil {
		t.Fatal("Batch Projection OriginScrapeID = nil")
	}

	service := projection.NewService(repository)
	if _, err := service.Move(ctx, projection.MoveRequest{
		Target:    "media:/Managed/Leaf/Dune.mkv",
		NewTarget: "media:/Moved/New/Renamed.mkv",
		Actor:     namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Move Batch-owned Projection: %v", err)
	}
	beforeRemove := readBatchOwnershipState(t, opened)
	removed, err := service.Remove(ctx, projection.RemoveRequest{
		Target: "media:/Moved/New/Renamed.mkv",
		Actor:  namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Remove Batch-owned Projection: %v", err)
	}
	if removed.Node.ID != before.ID ||
		removed.Node.OriginScrapeID == nil ||
		*removed.Node.OriginScrapeID != *before.OriginScrapeID ||
		removed.Target != "Moved/New/Renamed.mkv" {
		t.Errorf("removed Batch Projection = %#v, want node %d", removed, before.ID)
	}
	if _, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Moved/New/Renamed.mkv",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("removed target error = %v, want ErrNotFound", err)
	}
	if _, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Moved",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("removed managed directory error = %v, want ErrNotFound", err)
	}
	afterRemove := readBatchOwnershipState(t, opened)
	if afterRemove.entries != beforeRemove.entries ||
		afterRemove.version != beforeRemove.version+1 ||
		afterRemove.logs != beforeRemove.logs+1 {
		t.Errorf("state after remove = %#v, before = %#v", afterRemove, beforeRemove)
	}
	entries, err := repository.ProjectionBatchEntries(ctx, *before.OriginScrapeID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries after remove: %v", err)
	}
	if len(entries) != 1 ||
		entries[0].ProjectionNodeID != nil ||
		entries[0].Source.Path != sourcePath ||
		entries[0].Target != "Managed/Leaf/Dune.mkv" {
		t.Errorf("Batch entries after remove = %#v", entries)
	}
	detail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch after remove: %v", err)
	}
	if detail.Batch.Status != string(namespace.ProjectionBatchApplied) ||
		len(detail.Operations) != 3 ||
		detail.Operations[1].Name != "projection.move" ||
		detail.Operations[2].Name != "projection.remove" ||
		!reflect.DeepEqual(
			detail.Operations[2].Details.Paths,
			[]string{"media:/Moved/New/Renamed.mkv"},
		) {
		t.Errorf("Batch detail after remove = %#v", detail)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != string(sourceContent) {
		t.Errorf("source after remove = %q/%v", got, err)
	}

	undone, err := batchService.Undo(ctx, projectionbatch.UndoRequest{
		BatchID: applied.BatchID,
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		Caller:  projectionbatch.Caller{UID: 1000, GID: 1001},
	})
	if err != nil {
		t.Fatalf("Undo Batch after remove: %v", err)
	}
	if undone.Status != string(namespace.ProjectionBatchUndone) {
		t.Errorf("Undo status = %q, want undone", undone.Status)
	}
	if _, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("historical managed directory after Undo error = %v, want ErrNotFound", err)
	}
	undoneDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch after Undo: %v", err)
	}
	if undoneDetail.Batch.Status != string(namespace.ProjectionBatchUndone) ||
		len(undoneDetail.Entries) != 1 ||
		undoneDetail.Entries[0].ProjectionNodeID != nil ||
		len(undoneDetail.Operations) != 4 ||
		undoneDetail.Operations[2].Name != "projection.remove" ||
		undoneDetail.Operations[3].Name != "projection.batch.undo" {
		t.Errorf("Batch detail after Undo = %#v", undoneDetail)
	}
}

func TestRemoveBatchOwnedProjectionRollsBackAfterLogFailure(t *testing.T) {
	ctx := context.Background()
	opened := openBatchOwnershipDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createBatchOwnershipLibrary(t, repository)
	sourcePath := filepath.Join(t.TempDir(), "Dune.mkv")
	if err := os.WriteFile(sourcePath, []byte("batch source"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	batchService := projectionbatch.NewService(repository)
	data := batchOwnershipJSON(t, sourcePath, "Managed/Leaf/Dune.mkv")
	validation, err := batchService.Validate(ctx, data)
	if err != nil {
		t.Fatalf("Validate Batch: %v", err)
	}
	applied, err := batchService.Apply(ctx, projectionbatch.ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         projectionbatch.Caller{UID: 1000, GID: 1001},
	})
	if err != nil {
		t.Fatalf("Apply Batch: %v", err)
	}
	node, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed/Leaf/Dune.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup Batch Projection: %v", err)
	}
	if node.OriginScrapeID == nil {
		t.Fatal("Batch Projection OriginScrapeID = nil")
	}
	beforeEntries, err := repository.ProjectionBatchEntries(ctx, *node.OriginScrapeID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries before remove: %v", err)
	}
	beforeDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch before remove: %v", err)
	}
	beforeState := readBatchOwnershipState(t, opened)
	failBatchProjectionRemoveLog(t, opened)

	if _, err := projection.NewService(repository).Remove(ctx, projection.RemoveRequest{
		Target: "media:/Managed/Leaf/Dune.mkv",
		Actor:  namespace.Actor{Kind: namespace.ActorSystem},
	}); err == nil || !strings.Contains(err.Error(), "forced operation log failure") {
		t.Fatalf("Remove error = %v, want forced operation log failure", err)
	}
	current, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed/Leaf/Dune.mkv",
	)
	if err != nil || current.ID != node.ID {
		t.Errorf("Projection after rolled-back remove = %#v/%v", current, err)
	}
	afterEntries, err := repository.ProjectionBatchEntries(ctx, *node.OriginScrapeID)
	if err != nil {
		t.Fatalf("ProjectionBatchEntries after rolled-back remove: %v", err)
	}
	if !reflect.DeepEqual(afterEntries, beforeEntries) {
		t.Errorf("Batch entries changed: before=%#v after=%#v", beforeEntries, afterEntries)
	}
	afterDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch after rolled-back remove: %v", err)
	}
	if !reflect.DeepEqual(afterDetail, beforeDetail) {
		t.Errorf("Batch detail changed: before=%#v after=%#v", beforeDetail, afterDetail)
	}
	if afterState := readBatchOwnershipState(t, opened); afterState != beforeState {
		t.Errorf("database state changed: before=%#v after=%#v", beforeState, afterState)
	}
	for _, path := range []string{"Managed", "Managed/Leaf"} {
		if _, err := repository.Lookup(ctx, libraryRecord.RootNodeID, path); err != nil {
			t.Errorf("managed directory %q after rollback: %v", path, err)
		}
	}
}

func TestRebindRejectsBatchOwnedProjection(t *testing.T) {
	ctx := context.Background()
	opened := openBatchOwnershipDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createBatchOwnershipLibrary(t, repository)
	sourcePath := filepath.Join(t.TempDir(), "Dune.mkv")
	sourceContent := []byte("batch source")
	if err := os.WriteFile(sourcePath, sourceContent, 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	batchService := projectionbatch.NewService(repository)
	data := batchOwnershipJSON(t, sourcePath, "Managed/Leaf/Dune.mkv")
	validation, err := batchService.Validate(ctx, data)
	if err != nil {
		t.Fatalf("Validate Batch: %v", err)
	}
	applied, err := batchService.Apply(ctx, projectionbatch.ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         projectionbatch.Caller{UID: 1000, GID: 1001},
	})
	if err != nil {
		t.Fatalf("Apply Batch: %v", err)
	}
	node, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed/Leaf/Dune.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup Batch Projection: %v", err)
	}
	if node.OriginScrapeID == nil {
		t.Fatal("Batch Projection OriginScrapeID = nil")
	}
	beforeDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch before rebind: %v", err)
	}
	beforeState := readBatchOwnershipState(t, opened)

	_, err = projection.NewService(repository).Rebind(ctx, projection.RebindRequest{
		Target:     "media:/Managed/Leaf/Dune.mkv",
		SourcePath: filepath.Join(t.TempDir(), "must-not-be-opened.mkv"),
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	})
	if !errors.Is(err, projection.ErrBatchOwnedProjection) {
		t.Fatalf("Rebind error = %v, want ErrBatchOwnedProjection", err)
	}
	current, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Managed/Leaf/Dune.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup Batch Projection after rejected rebind: %v", err)
	}
	if current.ID != node.ID || current.Source == nil || *current.Source != *node.Source {
		t.Errorf("Batch Projection changed: before=%#v after=%#v", node, current)
	}
	afterDetail, err := batchService.Show(ctx, applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch after rejected rebind: %v", err)
	}
	if !reflect.DeepEqual(afterDetail, beforeDetail) {
		t.Errorf("Batch detail changed: before=%#v after=%#v", beforeDetail, afterDetail)
	}
	if afterState := readBatchOwnershipState(t, opened); afterState != beforeState {
		t.Errorf("database state changed: before=%#v after=%#v", beforeState, afterState)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != string(sourceContent) {
		t.Errorf("source after rejected rebind = %q/%v", got, err)
	}
}

type batchOwnershipState struct {
	nodes   int64
	entries int64
	version int64
	logs    int64
}

func openBatchOwnershipDatabase(t *testing.T) *database.DB {
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

func createBatchOwnershipLibrary(
	t *testing.T,
	repository *namespace.Repository,
) namespace.Library {
	t.Helper()
	record, err := library.NewService(repository).Create(
		context.Background(),
		library.CreateRequest{
			Name:    "media",
			Profile: library.ProfileIdentifier{Name: "jellyfin", Version: 1},
			Actor:   namespace.Actor{Kind: namespace.ActorSystem},
			UID:     1234,
			GID:     5678,
			Modes:   library.DefaultModes,
		},
	)
	if err != nil {
		t.Fatalf("Create Library: %v", err)
	}
	return record
}

func batchOwnershipJSON(t *testing.T, sourcePath, target string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"schema":  projectionbatch.BatchSchema,
		"library": "media",
		"entries": []map[string]string{
			{"source": sourcePath, "target": target},
		},
	})
	if err != nil {
		t.Fatalf("Marshal Batch: %v", err)
	}
	return data
}

func readBatchOwnershipState(t *testing.T, opened *database.DB) batchOwnershipState {
	t.Helper()
	var state batchOwnershipState
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		for query, destination := range map[string]*int64{
			"SELECT COUNT(*) FROM nodes":                            &state.nodes,
			"SELECT COUNT(*) FROM projection_batch_entries":         &state.entries,
			"SELECT version FROM namespace_state WHERE singleton=1": &state.version,
			"SELECT COUNT(*) FROM operation_log":                    &state.logs,
		} {
			if err := connection.QueryRowContext(
				context.Background(),
				query,
			).Scan(destination); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read database state: %v", err)
	}
	return state
}

func failBatchProjectionRemoveLog(t *testing.T, opened *database.DB) {
	t.Helper()
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(context.Background(), `
			CREATE TEMP TRIGGER fail_batch_projection_remove_log
			BEFORE INSERT ON operation_log
			WHEN NEW.operation = 'projection.remove'
			BEGIN
				SELECT RAISE(ABORT, 'forced operation log failure');
			END
		`); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
}
