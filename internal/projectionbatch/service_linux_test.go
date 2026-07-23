package projectionbatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
)

func TestServiceValidateIsStateless(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	service := NewService(repository)
	service.now = func() time.Time {
		return time.Unix(1_700_000_000, 123)
	}
	firstSource, secondSource := createBatchSources(t)
	data := batchJSON(t, firstSource, secondSource)

	result, err := service.Validate(context.Background(), data)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if result.Schema != ValidationSchema || !result.Valid {
		t.Errorf("validation result = %#v", result)
	}
	if len(result.Entries) != 2 ||
		result.Entries[0].Action != "create" ||
		result.Entries[0].Target != "media:/Movies/Dune/Dune.mkv" {
		t.Errorf("validation entries = %#v", result.Entries)
	}
	if result.Warnings == nil || len(result.Warnings) != 0 {
		t.Errorf("warnings = %#v, want empty array", result.Warnings)
	}
	if result.NamespaceVersion != 1 {
		t.Errorf("namespace version = %d, want 1", result.NamespaceVersion)
	}

	assertBatchDatabaseState(t, opened, 2, 0, 0, 1, 1)
}

func TestServiceValidateRejectsNamespaceConflictWithoutPersistence(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	firstSource, secondSource := createBatchSources(t)
	data := batchJSON(t, firstSource, secondSource)
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	direct := NewService(repository)
	if _, err := direct.Apply(context.Background(), ApplyRequest{
		Data:           singleEntryBatchJSON(t, firstSource, "Movies/Dune/Dune.mkv"),
		ExpectedDigest: mustParse(t, singleEntryBatchJSON(t, firstSource, "Movies/Dune/Dune.mkv")).Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         Caller{UID: 1, GID: 2},
	}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	_, err = NewService(repository).Validate(context.Background(), data)
	if !errors.Is(err, namespace.ErrConflict) {
		t.Fatalf("Validate conflict error = %v, want ErrConflict", err)
	}
	if parsed.Digest == "" {
		t.Fatal("parsed digest is empty")
	}
	assertBatchDatabaseState(t, opened, 5, 1, 1, 2, 2)
}

func TestServiceValidateRejectsTrustBoundaryInputsWithoutPersistence(t *testing.T) {
	tests := map[string]struct {
		prepare func(*testing.T, *database.DB, *namespace.Repository) []byte
		want    error
	}{
		"unsafe source": {
			prepare: func(t *testing.T, _ *database.DB, _ *namespace.Repository) []byte {
				root := t.TempDir()
				source := filepath.Join(root, "Dune.mkv")
				if err := os.WriteFile(source, []byte("video"), 0o600); err != nil {
					t.Fatalf("WriteFile source: %v", err)
				}
				link := filepath.Join(root, "Dune-link.mkv")
				if err := os.Symlink(source, link); err != nil {
					t.Fatalf("Symlink source: %v", err)
				}
				return singleEntryBatchJSON(t, link, "Movies/Dune.mkv")
			},
			want: projection.ErrUnsafeSource,
		},
		"inactive Library": {
			prepare: func(
				t *testing.T,
				opened *database.DB,
				_ *namespace.Repository,
			) []byte {
				if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
					transaction, err := connection.BeginTx(context.Background(), nil)
					if err != nil {
						return err
					}
					defer transaction.Rollback()
					if _, err := transaction.ExecContext(context.Background(), `
						UPDATE libraries
						SET status = 'RETIRED', root_node_id = NULL, retired_at = 1
						WHERE name = 'media'
					`); err != nil {
						return err
					}
					return transaction.Commit()
				}); err != nil {
					t.Fatalf("retire Library: %v", err)
				}
				source, _ := createBatchSources(t)
				return singleEntryBatchJSON(t, source, "Movies/Dune.mkv")
			},
			want: namespace.ErrLibraryNotFound,
		},
		"Profile path": {
			prepare: func(t *testing.T, _ *database.DB, _ *namespace.Repository) []byte {
				source, _ := createBatchSources(t)
				return singleEntryBatchJSON(t, source, "Movies/Dune: Part One.mkv")
			},
			want: namespace.ErrInvalidPath,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			opened := openBatchTestDatabase(t)
			repository := namespace.NewRepository(opened)
			createBatchTestLibrary(t, repository)
			data := test.prepare(t, opened, repository)

			_, err := NewService(repository).Validate(context.Background(), data)
			if !errors.Is(err, test.want) {
				t.Fatalf("Validate error = %v, want %v", err, test.want)
			}
			assertBatchDatabaseState(t, opened, 2, 0, 0, 1, 1)
		})
	}
}

func TestServiceApplyRevalidatesConflictsAfterValidate(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	firstSource, secondSource := createBatchSources(t)
	data := batchJSON(t, firstSource, secondSource)
	service := NewService(repository)
	validation, err := service.Validate(context.Background(), data)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := projection.NewService(repository).Create(
		context.Background(),
		projection.CreateRequest{
			SourcePath: firstSource,
			Target:     "media:/Movies/Dune/Dune.mkv",
			Actor:      namespace.Actor{Kind: namespace.ActorSystem},
		},
	); err != nil {
		t.Fatalf("create conflicting direct Projection: %v", err)
	}

	_, err = service.Apply(context.Background(), ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         Caller{UID: 1, GID: 2},
	})
	if !errors.Is(err, namespace.ErrConflict) {
		t.Fatalf("Apply error = %v, want ErrConflict", err)
	}
	assertBatchDatabaseState(t, opened, 5, 0, 0, 2, 2)
}

func TestServiceApplyDoesNotTreatDigestAsIdempotencyKey(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	firstSource, secondSource := createBatchSources(t)
	data := batchJSON(t, firstSource, secondSource)
	parsed := mustParse(t, data)
	service := NewService(repository)
	request := ApplyRequest{
		Data:           data,
		ExpectedDigest: parsed.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         Caller{UID: 1, GID: 2},
	}
	if _, err := service.Apply(context.Background(), request); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	_, err := service.Apply(context.Background(), request)
	if !errors.Is(err, namespace.ErrConflict) {
		t.Fatalf("second Apply error = %v, want ErrConflict", err)
	}
	assertBatchDatabaseState(t, opened, 6, 1, 2, 2, 2)
}

func TestServiceApplyCreatesBatchAtomicallyAndPersistsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	firstDatabase, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	firstRepository := namespace.NewRepository(firstDatabase)
	createBatchTestLibrary(t, firstRepository)
	firstSource, secondSource := createBatchSourcesIn(t, root)
	data := batchJSON(t, firstSource, secondSource)
	validation, err := NewService(firstRepository).Validate(ctx, data)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	actorID := int64(1000)
	result, err := NewService(firstRepository).Apply(ctx, ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorControl, ID: &actorID},
		Caller:         Caller{UID: 1000, GID: 1001},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Schema != ResultSchema ||
		result.BatchID != "batch-1" ||
		result.Digest != validation.Digest ||
		result.NamespaceVersion != 2 {
		t.Errorf("apply result = %#v", result)
	}

	for _, target := range []string{
		"Movies/Dune/Dune.mkv",
		"Movies/Dune/Dune.ass",
	} {
		node, err := firstRepository.Lookup(ctx, 2, target)
		if err != nil {
			t.Fatalf("Lookup %q: %v", target, err)
		}
		if node.OriginScrapeID == nil || *node.OriginScrapeID != 1 {
			t.Errorf("%q OriginScrapeID = %v, want 1", target, node.OriginScrapeID)
		}
	}
	assertBatchDatabaseState(t, firstDatabase, 6, 1, 2, 2, 2)

	list, err := NewService(firstRepository).List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Batches) != 1 || list.Batches[0].BatchID != result.BatchID {
		t.Errorf("list = %#v", list)
	}
	detail, err := NewService(firstRepository).Show(ctx, result.BatchID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(detail.Entries) != 2 ||
		detail.Entries[0].Source.Path != firstSource ||
		detail.Entries[1].Source.Path != secondSource {
		t.Errorf("detail entries = %#v", detail.Entries)
	}
	if len(detail.Operations) != 1 ||
		detail.Operations[0].Name != "projection.batch.apply" ||
		detail.Operations[0].NamespaceVersion != 2 {
		t.Errorf("detail operations = %#v", detail.Operations)
	}

	if err := firstDatabase.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	secondDatabase, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer secondDatabase.Close()

	reopened, err := NewService(namespace.NewRepository(secondDatabase)).Show(ctx, result.BatchID)
	if err != nil {
		t.Fatalf("Show after restart: %v", err)
	}
	if reopened.Batch != detail.Batch ||
		len(reopened.Entries) != len(detail.Entries) ||
		reopened.Entries[0].Source != detail.Entries[0].Source ||
		len(reopened.Operations) != len(detail.Operations) {
		t.Errorf("reopened detail = %#v, want %#v", reopened, detail)
	}
}

func TestServiceListAndShowRetainHistoryAfterLibraryRetirement(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	firstSource, secondSource := createBatchSources(t)
	data := batchJSON(t, firstSource, secondSource)
	parsed := mustParse(t, data)
	service := NewService(repository)
	applied, err := service.Apply(context.Background(), ApplyRequest{
		Data:           data,
		ExpectedDigest: parsed.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         Caller{UID: 1, GID: 2},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(context.Background(), `
			UPDATE libraries
			SET status = 'RETIRED', root_node_id = NULL, retired_at = 1
			WHERE name = 'media'
		`); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("retire Library: %v", err)
	}

	listed, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List after Library retirement: %v", err)
	}
	if len(listed.Batches) != 1 || listed.Batches[0].BatchID != applied.BatchID {
		t.Errorf("List = %#v", listed)
	}
	shown, err := service.Show(context.Background(), applied.BatchID)
	if err != nil {
		t.Fatalf("Show after Library retirement: %v", err)
	}
	if len(shown.Entries) != 2 || shown.Entries[0].Source.Path != firstSource {
		t.Errorf("Show = %#v", shown)
	}
	_, err = service.Undo(context.Background(), UndoRequest{
		BatchID: applied.BatchID,
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		Caller:  Caller{UID: 1, GID: 2},
	})
	if !errors.Is(err, namespace.ErrLibraryNotFound) {
		t.Fatalf("Undo retired Library error = %v, want ErrLibraryNotFound", err)
	}
}

func TestServiceShowReturnsOneAtomicBatchSnapshotDuringUndo(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	source, _ := createBatchSources(t)
	service := NewService(repository)

	for index := range 50 {
		target := fmt.Sprintf("Movies/Dune-%02d.mkv", index)
		data := singleEntryBatchJSON(t, source, target)
		parsed := mustParse(t, data)
		applied, err := service.Apply(context.Background(), ApplyRequest{
			Data:           data,
			ExpectedDigest: parsed.Digest,
			Actor:          namespace.Actor{Kind: namespace.ActorSystem},
			Caller:         Caller{UID: 1, GID: 2},
		})
		if err != nil {
			t.Fatalf("Apply %d: %v", index, err)
		}

		start := make(chan struct{})
		showResult := make(chan DetailResult, 1)
		showError := make(chan error, 1)
		undoError := make(chan error, 1)
		go func() {
			<-start
			result, err := service.Show(context.Background(), applied.BatchID)
			showResult <- result
			showError <- err
		}()
		go func() {
			<-start
			_, err := service.Undo(context.Background(), UndoRequest{
				BatchID: applied.BatchID,
				Actor:   namespace.Actor{Kind: namespace.ActorSystem},
				Caller:  Caller{UID: 1, GID: 2},
			})
			undoError <- err
		}()
		close(start)

		result := <-showResult
		if err := <-showError; err != nil {
			t.Fatalf("Show %d: %v", index, err)
		}
		if err := <-undoError; err != nil {
			t.Fatalf("Undo %d: %v", index, err)
		}
		switch result.Batch.Status {
		case "applied":
			if len(result.Entries) != 1 ||
				result.Entries[0].ProjectionNodeID == nil ||
				len(result.Operations) != 1 {
				t.Fatalf("mixed applied snapshot %d: %#v", index, result)
			}
		case "undone":
			if len(result.Entries) != 1 ||
				result.Entries[0].ProjectionNodeID != nil ||
				len(result.Operations) != 2 {
				t.Fatalf("mixed undone snapshot %d: %#v", index, result)
			}
		default:
			t.Fatalf("Show status %q", result.Batch.Status)
		}
	}
}

func TestServiceApplyRejectsDigestMismatchBeforeMutation(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	firstSource, secondSource := createBatchSources(t)

	_, err := NewService(repository).Apply(context.Background(), ApplyRequest{
		Data:           batchJSON(t, firstSource, secondSource),
		ExpectedDigest: "sha256:" + strings.Repeat("0", 64),
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         Caller{UID: 1, GID: 2},
	})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Apply error = %v, want ErrDigestMismatch", err)
	}
	assertBatchDatabaseState(t, opened, 2, 0, 0, 1, 1)
}

func TestServiceApplyRollsBackBatchNodesLogAndVersion(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createBatchTestLibrary(t, repository)
	firstSource, secondSource := createBatchSources(t)
	data := batchJSON(t, firstSource, secondSource)
	parsed := mustParse(t, data)

	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(context.Background(), `
			CREATE TEMP TRIGGER fail_batch_log
			BEFORE INSERT ON operation_log
			WHEN NEW.operation = 'projection.batch.apply'
			BEGIN
				SELECT RAISE(ABORT, 'forced batch log failure');
			END
		`); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	_, err := NewService(repository).Apply(context.Background(), ApplyRequest{
		Data:           data,
		ExpectedDigest: parsed.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         Caller{UID: 1, GID: 2},
	})
	if err == nil || !strings.Contains(err.Error(), "forced batch log failure") {
		t.Fatalf("Apply error = %v, want forced operation-log failure", err)
	}
	assertBatchDatabaseState(t, opened, 2, 0, 0, 1, 1)
}

func TestServiceUndoPreservesOverlayReportsItAndRejectsRepeat(t *testing.T) {
	opened := openBatchTestDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createBatchTestLibrary(t, repository)
	firstSource, secondSource := createBatchSources(t)
	data := batchJSON(t, firstSource, secondSource)
	parsed := mustParse(t, data)
	service := NewService(repository)

	applied, err := service.Apply(context.Background(), ApplyRequest{
		Data:           data,
		ExpectedDigest: parsed.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         Caller{UID: 1, GID: 2},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	parent, err := repository.Lookup(context.Background(), libraryRecord.RootNodeID, "Movies/Dune")
	if err != nil {
		t.Fatalf("Lookup parent: %v", err)
	}
	objectID := createObject(t, opened)
	if _, err := repository.Update(context.Background(), namespace.Operation{
		Name:  "overlay.create",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
		Details: namespace.OperationDetails{
			Paths:     []string{"media:/Movies/Dune/poster.jpg"},
			NodeKinds: []namespace.NodeKind{namespace.OverlayFile},
		},
	}, func(transaction *namespace.Transaction) error {
		_, err := transaction.CreateNode(context.Background(), namespace.CreateNode{
			ParentID: parent.ID,
			Name:     "poster.jpg",
			Kind:     namespace.OverlayFile,
			ObjectID: &objectID,
			Creator:  namespace.Actor{Kind: namespace.ActorSystem},
			Attributes: namespace.Attributes{
				Mode: libraryRecord.OverlayMode,
				UID:  libraryRecord.UID,
				GID:  libraryRecord.GID,
			},
		})
		return err
	}); err != nil {
		t.Fatalf("create OverlayFile: %v", err)
	}
	alreadyRemoved, err := repository.Lookup(
		context.Background(),
		libraryRecord.RootNodeID,
		"Movies/Dune/Dune.ass",
	)
	if err != nil {
		t.Fatalf("Lookup Projection for early removal: %v", err)
	}
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(
			context.Background(),
			"DELETE FROM nodes WHERE id = ?",
			alreadyRemoved.ID,
		); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("remove Projection before undo: %v", err)
	}

	result, err := service.Undo(context.Background(), UndoRequest{
		BatchID: applied.BatchID,
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		Caller:  Caller{UID: 10, GID: 11},
	})
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if result.Schema != UndoSchema ||
		result.Status != "undone" ||
		len(result.RetainedOverlayFiles) != 1 ||
		result.RetainedOverlayFiles[0] != "media:/Movies/Dune/poster.jpg" {
		t.Errorf("undo result = %#v", result)
	}
	if _, err := repository.Lookup(
		context.Background(),
		libraryRecord.RootNodeID,
		"Movies/Dune/Dune.mkv",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Fatalf("Lookup removed Projection error = %v, want ErrNotFound", err)
	}
	if _, err := repository.Lookup(
		context.Background(),
		libraryRecord.RootNodeID,
		"Movies/Dune/poster.jpg",
	); err != nil {
		t.Fatalf("Lookup retained OverlayFile: %v", err)
	}
	detail, err := service.Show(context.Background(), applied.BatchID)
	if err != nil {
		t.Fatalf("Show undone batch: %v", err)
	}
	if detail.Batch.Status != "undone" ||
		detail.Entries[0].ProjectionNodeID != nil ||
		detail.Entries[0].Source.Path != firstSource {
		t.Errorf("undone detail = %#v", detail)
	}
	if len(detail.Operations) != 2 ||
		detail.Operations[1].Name != "projection.batch.undo" {
		t.Errorf("undone operations = %#v", detail.Operations)
	}

	_, err = service.Undo(context.Background(), UndoRequest{
		BatchID: applied.BatchID,
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		Caller:  Caller{UID: 10, GID: 11},
	})
	if !errors.Is(err, ErrAlreadyUndone) {
		t.Fatalf("repeated Undo error = %v, want ErrAlreadyUndone", err)
	}
}

func openBatchTestDatabase(t *testing.T) *database.DB {
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

func createBatchTestLibrary(
	t *testing.T,
	repository *namespace.Repository,
) namespace.Library {
	t.Helper()

	created, err := library.NewService(repository).Create(context.Background(), library.CreateRequest{
		Name:    "media",
		Profile: library.ProfileIdentifier{Name: "jellyfin", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1234,
		GID:     5678,
		Modes:   library.DefaultModes,
	})
	if err != nil {
		t.Fatalf("Create Library: %v", err)
	}
	return created
}

func createBatchSources(t *testing.T) (string, string) {
	t.Helper()
	return createBatchSourcesIn(t, t.TempDir())
}

func createBatchSourcesIn(t *testing.T, root string) (string, string) {
	t.Helper()

	first := filepath.Join(root, "Dune.mkv")
	second := filepath.Join(root, "Dune.ass")
	if err := os.WriteFile(first, []byte("video"), 0o600); err != nil {
		t.Fatalf("WriteFile first source: %v", err)
	}
	if err := os.WriteFile(second, []byte("subtitle"), 0o600); err != nil {
		t.Fatalf("WriteFile second source: %v", err)
	}
	return first, second
}

func batchJSON(t *testing.T, firstSource, secondSource string) []byte {
	t.Helper()
	document := Document{
		Schema:  BatchSchema,
		Library: "media",
		Entries: []Entry{
			{Source: firstSource, Target: "Movies/Dune/Dune.mkv"},
			{Source: secondSource, Target: "Movies/Dune/Dune.ass"},
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal batch: %v", err)
	}
	return data
}

func singleEntryBatchJSON(t *testing.T, source, target string) []byte {
	t.Helper()
	data, err := json.Marshal(Document{
		Schema:  BatchSchema,
		Library: "media",
		Entries: []Entry{{Source: source, Target: target}},
	})
	if err != nil {
		t.Fatalf("Marshal single-entry batch: %v", err)
	}
	return data
}

func mustParse(t *testing.T, data []byte) Parsed {
	t.Helper()
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return parsed
}

func assertBatchDatabaseState(
	t *testing.T,
	opened *database.DB,
	wantNodes, wantBatches, wantEntries, wantVersion, wantLogs int64,
) {
	t.Helper()

	var nodes int64
	var batches int64
	var entries int64
	var version int64
	var logs int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		for query, destination := range map[string]*int64{
			"SELECT COUNT(*) FROM nodes":                            &nodes,
			"SELECT COUNT(*) FROM scrapes WHERE status IS NOT NULL": &batches,
			"SELECT COUNT(*) FROM projection_batch_entries":         &entries,
			"SELECT version FROM namespace_state WHERE singleton=1": &version,
			"SELECT COUNT(*) FROM operation_log":                    &logs,
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
		t.Fatalf("read batch database state: %v", err)
	}
	if nodes != wantNodes ||
		batches != wantBatches ||
		entries != wantEntries ||
		version != wantVersion ||
		logs != wantLogs {
		t.Errorf(
			"database state = nodes %d batches %d entries %d version %d logs %d, want %d %d %d %d %d",
			nodes,
			batches,
			entries,
			version,
			logs,
			wantNodes,
			wantBatches,
			wantEntries,
			wantVersion,
			wantLogs,
		)
	}
}

func createObject(t *testing.T, opened *database.DB) int64 {
	t.Helper()

	var objectID int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		result, err := transaction.ExecContext(
			context.Background(),
			`INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			 VALUES ('LIVE', 1, 1, 1)`,
		)
		if err != nil {
			return err
		}
		objectID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("create Object: %v", err)
	}
	return objectID
}
