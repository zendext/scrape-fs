package projection_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func TestDirectRemoveAndRebindRejectBatchOwnedProjection(t *testing.T) {
	for _, operation := range []string{"remove", "rebind"} {
		t.Run(operation, func(t *testing.T) {
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
				t.Fatalf("Show Batch before lifecycle attempt: %v", err)
			}
			beforeEntries, err := repository.ProjectionBatchEntries(ctx, *node.OriginScrapeID)
			if err != nil {
				t.Fatalf("ProjectionBatchEntries before lifecycle attempt: %v", err)
			}
			beforeState := readBatchOwnershipState(t, opened)

			replacementPath := filepath.Join(t.TempDir(), "replacement.mkv")
			if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
				t.Fatalf("WriteFile replacement: %v", err)
			}
			serviceSourcePath := replacementPath
			if operation == "rebind" {
				serviceSourcePath = filepath.Join(t.TempDir(), "must-not-be-opened.mkv")
			}
			service := projection.NewService(repository)
			switch operation {
			case "remove":
				_, err = service.Remove(ctx, projection.RemoveRequest{
					Target: "media:/Managed/Leaf/Dune.mkv",
					Actor:  namespace.Actor{Kind: namespace.ActorSystem},
				})
			case "rebind":
				_, err = service.Rebind(ctx, projection.RebindRequest{
					Target:     "media:/Managed/Leaf/Dune.mkv",
					SourcePath: serviceSourcePath,
					Actor:      namespace.Actor{Kind: namespace.ActorSystem},
				})
			}
			if !errors.Is(err, projection.ErrBatchOwnedProjection) {
				t.Fatalf(
					"%s Batch-owned Projection error = %v, want ErrBatchOwnedProjection",
					operation,
					err,
				)
			}

			_, err = repository.Update(ctx, namespace.Operation{
				Name:  "test.projection." + operation,
				Actor: namespace.Actor{Kind: namespace.ActorSystem},
				Details: namespace.OperationDetails{
					Paths:     []string{"media:/Managed/Leaf/Dune.mkv"},
					NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
				},
			}, func(transaction *namespace.Transaction) error {
				switch operation {
				case "remove":
					_, err := transaction.RemoveProjection(ctx, namespace.RemoveProjectionNode{
						ParentID:   *node.ParentID,
						Name:       node.Name,
						ExpectedID: node.ID,
					})
					return err
				case "rebind":
					replacement := *node.Source
					replacement.Path = replacementPath
					_, err := transaction.RebindProjection(
						ctx,
						namespace.RebindProjectionNode{
							ParentID:       *node.ParentID,
							Name:           node.Name,
							ExpectedID:     node.ID,
							ExpectedSource: *node.Source,
							Source:         replacement,
						},
					)
					return err
				default:
					t.Fatalf("unknown operation %q", operation)
					return nil
				}
			})
			if !errors.Is(err, namespace.ErrBatchOwnedProjection) {
				t.Fatalf(
					"Transaction %s error = %v, want ErrBatchOwnedProjection",
					operation,
					err,
				)
			}

			current, err := repository.Lookup(
				ctx,
				libraryRecord.RootNodeID,
				"Managed/Leaf/Dune.mkv",
			)
			if err != nil {
				t.Fatalf("Lookup Batch Projection after rejected lifecycle: %v", err)
			}
			if current.ID != node.ID ||
				current.OriginScrapeID == nil ||
				*current.OriginScrapeID != *node.OriginScrapeID ||
				current.Source == nil ||
				node.Source == nil ||
				*current.Source != *node.Source {
				t.Errorf("Batch Projection changed: before=%#v after=%#v", node, current)
			}
			for _, path := range []string{"Managed", "Managed/Leaf"} {
				if _, err := repository.Lookup(ctx, libraryRecord.RootNodeID, path); err != nil {
					t.Errorf("managed directory %q after rejected lifecycle: %v", path, err)
				}
			}
			afterEntries, err := repository.ProjectionBatchEntries(ctx, *node.OriginScrapeID)
			if err != nil {
				t.Fatalf("ProjectionBatchEntries after rejected lifecycle: %v", err)
			}
			if !reflect.DeepEqual(afterEntries, beforeEntries) {
				t.Errorf("Batch entries changed: before=%#v after=%#v", beforeEntries, afterEntries)
			}
			afterDetail, err := batchService.Show(ctx, applied.BatchID)
			if err != nil {
				t.Fatalf("Show Batch after rejected lifecycle: %v", err)
			}
			if !reflect.DeepEqual(afterDetail, beforeDetail) {
				t.Errorf("Batch detail changed: before=%#v after=%#v", beforeDetail, afterDetail)
			}
			if afterState := readBatchOwnershipState(t, opened); afterState != beforeState {
				t.Errorf("database state changed: before=%#v after=%#v", beforeState, afterState)
			}
			if got, err := os.ReadFile(sourcePath); err != nil || string(got) != string(sourceContent) {
				t.Errorf("source after rejected lifecycle = %q, %v", got, err)
			}

			undone, err := batchService.Undo(ctx, projectionbatch.UndoRequest{
				BatchID: applied.BatchID,
				Actor:   namespace.Actor{Kind: namespace.ActorSystem},
				Caller:  projectionbatch.Caller{UID: 1000, GID: 1001},
			})
			if err != nil {
				t.Fatalf("Undo Batch after rejected lifecycle: %v", err)
			}
			if undone.Status != string(namespace.ProjectionBatchUndone) {
				t.Errorf("Undo status = %q, want undone", undone.Status)
			}
			if _, err := repository.Lookup(
				ctx,
				libraryRecord.RootNodeID,
				"Managed/Leaf/Dune.mkv",
			); !errors.Is(err, namespace.ErrNotFound) {
				t.Errorf("Batch Projection after Undo error = %v, want ErrNotFound", err)
			}
			if _, err := repository.Lookup(
				ctx,
				libraryRecord.RootNodeID,
				"Managed",
			); !errors.Is(err, namespace.ErrNotFound) {
				t.Errorf("managed directory after Undo error = %v, want ErrNotFound", err)
			}
			undoneDetail, err := batchService.Show(ctx, applied.BatchID)
			if err != nil {
				t.Fatalf("Show Batch after Undo: %v", err)
			}
			if undoneDetail.Batch.Status != string(namespace.ProjectionBatchUndone) ||
				len(undoneDetail.Entries) != 1 ||
				undoneDetail.Entries[0].ProjectionNodeID != nil ||
				len(undoneDetail.Operations) != 2 {
				t.Errorf("Batch detail after Undo = %#v", undoneDetail)
			}
		})
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
