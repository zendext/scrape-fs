package library

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
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
