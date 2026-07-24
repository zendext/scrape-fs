package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestServiceCreateAndListLibraries(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()

	plex, err := service.Create(ctx, CreateRequest{
		Name:    "plex",
		Profile: ProfileIdentifier{Name: "plex", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1000,
		GID:     1001,
		Modes: Modes{
			Root:       0o775,
			Directory:  0o750,
			Projection: 0o440,
			Overlay:    0o660,
		},
	})
	if err != nil {
		t.Fatalf("Create plex: %v", err)
	}
	jellyfin, err := service.Create(ctx, CreateRequest{
		Name:    "jellyfin",
		Profile: ProfileIdentifier{Name: "jellyfin", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     2000,
		GID:     2001,
		Modes:   DefaultModes,
	})
	if err != nil {
		t.Fatalf("Create jellyfin: %v", err)
	}

	libraries, err := service.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(libraries) != 2 {
		t.Fatalf("List count = %d, want 2", len(libraries))
	}
	if libraries[0] != jellyfin || libraries[1] != plex {
		t.Errorf("List = %#v, want name-ordered [%#v %#v]", libraries, jellyfin, plex)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
}

func TestServiceGetLibraryByName(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()

	created, err := service.Create(ctx, CreateRequest{
		Name:    "jellyfin",
		Profile: ProfileIdentifier{Name: "jellyfin", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1000,
		GID:     1000,
		Modes:   DefaultModes,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	found, err := service.Get(ctx, "jellyfin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found != created {
		t.Errorf("Get = %#v, want %#v", found, created)
	}
	if _, err := service.Get(ctx, "missing"); !errors.Is(err, namespace.ErrLibraryNotFound) {
		t.Fatalf("Get missing error = %v, want namespace.ErrLibraryNotFound", err)
	}
}

func TestServiceCreateRejectsMissingProfileWithoutMutation(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))

	_, err := service.Create(context.Background(), CreateRequest{
		Name:    "missing-profile",
		Profile: ProfileIdentifier{Name: "jellyfin", Version: 2},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1000,
		GID:     1000,
		Modes:   DefaultModes,
	})
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("Create error = %v, want ErrProfileNotFound", err)
	}

	libraries, listErr := service.List(context.Background())
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(libraries) != 0 {
		t.Errorf("List = %#v, want empty", libraries)
	}
	assertVersionAndLogCount(t, opened, 0, 0)
}

func TestServiceCreateRejectsDuplicateActiveNameWithoutPartialState(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	request := CreateRequest{
		Name:    "library",
		Profile: ProfileIdentifier{Name: "jellyfin", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1000,
		GID:     1000,
		Modes:   DefaultModes,
	}

	first, err := service.Create(ctx, request)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := service.Create(ctx, request); !errors.Is(err, namespace.ErrConflict) {
		t.Fatalf("second Create error = %v, want namespace.ErrConflict", err)
	}

	libraries, err := service.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(libraries) != 1 || libraries[0] != first {
		t.Errorf("List = %#v, want only %#v", libraries, first)
	}
	var rootCount int64
	if err := opened.UseConnection(ctx, func(connection *database.Connection) error {
		return connection.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM nodes WHERE kind = 'LIBRARY_ROOT'",
		).Scan(&rootCount)
	}); err != nil {
		t.Fatalf("count roots: %v", err)
	}
	if rootCount != 1 {
		t.Errorf("LibraryRoot count = %d, want 1", rootCount)
	}
	assertVersionAndLogCount(t, opened, 1, 1)
}

func TestServiceListPersistsLibraryConfigurationAcrossRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	firstDatabase, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	firstService := NewService(namespace.NewRepository(firstDatabase))
	created, err := firstService.Create(ctx, CreateRequest{
		Name:    "persisted",
		Profile: ProfileIdentifier{Name: "plex", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1234,
		GID:     5678,
		Modes: Modes{
			Root:       0o751,
			Directory:  0o750,
			Projection: 0o440,
			Overlay:    0o640,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := firstDatabase.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	secondDatabase, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer secondDatabase.Close()

	libraries, err := NewService(namespace.NewRepository(secondDatabase)).List(ctx)
	if err != nil {
		t.Fatalf("List after restart: %v", err)
	}
	if len(libraries) != 1 || libraries[0] != created {
		t.Errorf("List after restart = %#v, want %#v", libraries, created)
	}
	assertVersionAndLogCount(t, secondDatabase, 1, 1)
}

func TestServiceRenameLibraryAtomicallyPreservesStableIDs(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")

	renamed, err := service.Rename(ctx, RenameRequest{
		Name:    "media",
		NewName: "movies",
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.ID != created.ID || renamed.RootNodeID != created.RootNodeID {
		t.Errorf("Rename changed stable IDs: got %#v, created %#v", renamed, created)
	}
	if renamed.Name != "movies" {
		t.Errorf("Rename name = %q, want movies", renamed.Name)
	}
	if _, err := service.Get(ctx, "media"); !errors.Is(err, namespace.ErrLibraryNotFound) {
		t.Errorf("Get old name error = %v, want ErrLibraryNotFound", err)
	}
	root, err := namespace.NewRepository(opened).Node(ctx, created.RootNodeID)
	if err != nil {
		t.Fatalf("read renamed LibraryRoot: %v", err)
	}
	if root.ID != created.RootNodeID || root.Name != "movies" {
		t.Errorf("renamed LibraryRoot = %#v", root)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
	assertLastOperation(t, opened, "library.rename", 2)
}

func TestServiceRenameConflictLeavesStateUnchanged(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	first := createTestLibrary(t, service, "first", "jellyfin")
	second := createTestLibrary(t, service, "second", "plex")

	if _, err := service.Rename(ctx, RenameRequest{
		Name:    "first",
		NewName: "second",
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Rename error = %v, want ErrAlreadyExists", err)
	}

	libraries, err := service.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(libraries) != 2 || libraries[0] != first || libraries[1] != second {
		t.Errorf("Libraries after conflict = %#v, want unchanged", libraries)
	}
	assertVersionAndLogCount(t, opened, 2, 2)
}

func TestServiceRenameRejectsInvalidNameWithoutMutation(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")

	for _, newName := range []string{"", "../movies"} {
		if _, err := service.Rename(ctx, RenameRequest{
			Name:    "media",
			NewName: newName,
			Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		}); !errors.Is(err, namespace.ErrInvalidLibrary) {
			t.Fatalf("Rename(%q) error = %v, want ErrInvalidLibrary", newName, err)
		}
	}
	found, err := service.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found != created {
		t.Errorf("Library after invalid rename = %#v, want %#v", found, created)
	}
	assertVersionAndLogCount(t, opened, 1, 1)
}

func TestServiceRenameRollsBackWhenOperationLogFails(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")
	injectOperationFailure(t, opened, "library.rename")

	if _, err := service.Rename(ctx, RenameRequest{
		Name:    "media",
		NewName: "movies",
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	}); err == nil {
		t.Fatal("Rename succeeded with forced operation-log failure")
	}

	found, err := service.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get original Library: %v", err)
	}
	if found != created {
		t.Errorf("Library after rollback = %#v, want %#v", found, created)
	}
	root, err := namespace.NewRepository(opened).Node(ctx, created.RootNodeID)
	if err != nil {
		t.Fatalf("read LibraryRoot: %v", err)
	}
	if root.Name != "media" {
		t.Errorf("LibraryRoot name after rollback = %q, want media", root.Name)
	}
	assertVersionAndLogCount(t, opened, 1, 1)
}

func TestServiceConcurrentRenameToSameNameHasOneWinner(t *testing.T) {
	opened := openTestDatabase(t)
	firstService := NewService(namespace.NewRepository(opened))
	secondService := NewService(namespace.NewRepository(opened))
	createTestLibrary(t, firstService, "first", "jellyfin")
	createTestLibrary(t, firstService, "second", "plex")

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	rename := func(service *Service, name string) {
		ready.Done()
		<-start
		_, err := service.Rename(context.Background(), RenameRequest{
			Name:    name,
			NewName: "shared",
			Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		})
		results <- err
	}
	go rename(firstService, "first")
	go rename(secondService, "second")
	ready.Wait()
	close(start)

	var successes int
	var conflicts int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyExists):
			conflicts++
		default:
			t.Fatalf("concurrent Rename error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results = %d successes, %d conflicts", successes, conflicts)
	}

	libraries, err := firstService.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(libraries) != 2 || libraries[1].Name != "shared" {
		t.Errorf("Libraries after concurrent rename = %#v", libraries)
	}
	assertVersionAndLogCount(t, opened, 3, 3)
}

func TestServiceSetProfileAuditsWithoutChangingNamespaceVersion(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	service := NewService(repository)
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")
	sourceRoot := t.TempDir()
	existingSource := filepath.Join(sourceRoot, "existing.mkv")
	if err := writeTestFile(existingSource); err != nil {
		t.Fatalf("write existing source: %v", err)
	}
	existing, err := projection.NewService(repository).Create(ctx, projection.CreateRequest{
		SourcePath: existingSource,
		Target:     "media:/Movies/Existing.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Create existing Projection: %v", err)
	}

	updated, err := service.SetProfile(ctx, SetProfileRequest{
		Name:    "media",
		Profile: ProfileIdentifier{Name: "plex", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	if updated.ID != created.ID ||
		updated.RootNodeID != created.RootNodeID ||
		updated.ProfileName != "plex" ||
		updated.ProfileVersion != 1 {
		t.Errorf("SetProfile result = %#v, created %#v", updated, created)
	}
	persisted, err := repository.Lookup(ctx, created.RootNodeID, "Movies/Existing.mkv")
	if err != nil {
		t.Fatalf("Lookup existing Projection after Profile change: %v", err)
	}
	if persisted.ID != existing.ID {
		t.Errorf("existing Projection ID = %d, want %d", persisted.ID, existing.ID)
	}
	assertVersionAndLogCount(t, opened, 2, 3)
	assertLastOperation(t, opened, "library.set-profile", 2)

	source := filepath.Join(sourceRoot, "movie.mkv")
	if err := writeTestFile(source); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := projection.NewService(repository).Create(ctx, projection.CreateRequest{
		SourcePath: source,
		Target:     `media:/Movies/Title?.mkv`,
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create with updated plex Profile: %v", err)
	}
	batch := []byte(fmt.Sprintf(
		`{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":%q,"target":"Movies/Batch?.mkv"}]}`,
		source,
	))
	batchService := projectionbatch.NewService(repository)
	validated, err := batchService.Validate(ctx, batch)
	if err != nil {
		t.Fatalf("Validate Batch with updated plex Profile: %v", err)
	}
	applied, err := batchService.Apply(ctx, projectionbatch.ApplyRequest{
		Data:           batch,
		ExpectedDigest: validated.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         projectionbatch.Caller{UID: 1000, GID: 1000},
	})
	if err != nil {
		t.Fatalf("Apply Batch with updated plex Profile: %v", err)
	}
	if applied.NamespaceVersion != 4 {
		t.Errorf("Batch namespace version = %d, want 4", applied.NamespaceVersion)
	}
	assertVersionAndLogCount(t, opened, 4, 5)
}

func TestServiceSetProfileRejectsUnavailableProfileWithoutMutation(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")

	if _, err := service.SetProfile(ctx, SetProfileRequest{
		Name:    "media",
		Profile: ProfileIdentifier{Name: "jellyfin", Version: 2},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	}); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("SetProfile error = %v, want ErrProfileNotFound", err)
	}
	found, err := service.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found != created {
		t.Errorf("Library after rejected Profile = %#v, want %#v", found, created)
	}
	assertVersionAndLogCount(t, opened, 1, 1)
}

func TestServiceSetProfileRollsBackWhenOperationLogFails(t *testing.T) {
	opened := openTestDatabase(t)
	service := NewService(namespace.NewRepository(opened))
	ctx := context.Background()
	created := createTestLibrary(t, service, "media", "jellyfin")
	injectOperationFailure(t, opened, "library.set-profile")

	if _, err := service.SetProfile(ctx, SetProfileRequest{
		Name:    "media",
		Profile: ProfileIdentifier{Name: "plex", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	}); err == nil {
		t.Fatal("SetProfile succeeded with forced operation-log failure")
	}
	found, err := service.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found != created {
		t.Errorf("Library after rollback = %#v, want %#v", found, created)
	}
	assertVersionAndLogCount(t, opened, 1, 1)
}

func TestServiceRenameAndProfilePersistAcrossRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	firstDatabase, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	firstService := NewService(namespace.NewRepository(firstDatabase))
	created := createTestLibrary(t, firstService, "media", "jellyfin")
	renamed, err := firstService.Rename(ctx, RenameRequest{
		Name:    "media",
		NewName: "movies",
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	updated, err := firstService.SetProfile(ctx, SetProfileRequest{
		Name:    "movies",
		Profile: ProfileIdentifier{Name: "plex", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	if err := firstDatabase.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	secondDatabase, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer secondDatabase.Close()

	found, err := NewService(namespace.NewRepository(secondDatabase)).Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if found != updated || found.ID != created.ID || found.RootNodeID != renamed.RootNodeID {
		t.Errorf("Library after restart = %#v, want %#v", found, updated)
	}
	assertVersionAndLogCount(t, secondDatabase, 2, 3)
}

func createTestLibrary(
	t *testing.T,
	service *Service,
	name string,
	profile string,
) namespace.Library {
	t.Helper()
	created, err := service.Create(context.Background(), CreateRequest{
		Name:    name,
		Profile: ProfileIdentifier{Name: profile, Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1000,
		GID:     1000,
		Modes:   DefaultModes,
	})
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return created
}

func injectOperationFailure(t *testing.T, opened *database.DB, operation string) {
	t.Helper()
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(context.Background(), fmt.Sprintf(`
			CREATE TEMP TRIGGER fail_library_operation
			BEFORE INSERT ON operation_log
			WHEN NEW.operation = '%s'
			BEGIN
				SELECT RAISE(ABORT, 'forced operation-log failure');
			END
		`, operation)); err != nil {
			transaction.Rollback()
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("inject operation failure: %v", err)
	}
}

func assertLastOperation(t *testing.T, opened *database.DB, wantName string, wantVersion int64) {
	t.Helper()
	var name string
	var version int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(context.Background(), `
			SELECT operation, namespace_version
			FROM operation_log
			ORDER BY id DESC
			LIMIT 1
		`).Scan(&name, &version)
	}); err != nil {
		t.Fatalf("read last operation: %v", err)
	}
	if name != wantName || version != wantVersion {
		t.Errorf(
			"last operation = %q at version %d, want %q at version %d",
			name,
			version,
			wantName,
			wantVersion,
		)
	}
}

func writeTestFile(path string) error {
	return os.WriteFile(path, []byte("source"), 0o600)
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
			"namespace state = version %d logs %d, want version %d logs %d",
			version,
			logs,
			wantVersion,
			wantLogs,
		)
	}
}
