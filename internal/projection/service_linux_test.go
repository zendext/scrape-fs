package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

func TestServiceCreateProjectionAtomically(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createTestLibrary(t, repository)
	service := NewService(repository)
	service.now = func() time.Time {
		return time.Unix(1_700_000_000, 123_456_789)
	}

	sourcePath := filepath.Join(t.TempDir(), "Dune.mkv")
	sourceContent := []byte("original media")
	if err := os.WriteFile(sourcePath, sourceContent, 0o640); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	before := statSource(t, sourcePath)

	created, err := service.Create(context.Background(), CreateRequest{
		SourcePath: sourcePath,
		Target:     "media:/Movies/Dune (2021)/Dune.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if created.Kind != namespace.ProjectionFile {
		t.Errorf("Kind = %q, want ProjectionFile", created.Kind)
	}
	if created.Name != "Dune.mkv" {
		t.Errorf("Name = %q, want Dune.mkv", created.Name)
	}
	if created.Source == nil {
		t.Fatal("Source = nil")
	}
	if created.Source.Path != sourcePath ||
		created.Source.DeviceID != int64(before.Dev) ||
		created.Source.Inode != int64(before.Ino) ||
		created.Source.Size != before.Size ||
		created.Source.AtimeNS != before.Atim.Nano() ||
		created.Source.MtimeNS != before.Mtim.Nano() ||
		created.Source.SourceCtimeNS != before.Ctim.Nano() ||
		created.Source.VerifiedAtNS != service.now().UTC().UnixNano() {
		t.Errorf("Source = %#v, want metadata from %#v", created.Source, before)
	}
	if created.Attributes != (namespace.Attributes{
		Mode: libraryRecord.ProjectionMode,
		UID:  libraryRecord.UID,
		GID:  libraryRecord.GID,
	}) {
		t.Errorf("Attributes = %#v, want Library projection attributes", created.Attributes)
	}
	if created.Attributes.Mode.Perm()&0o222 != 0 {
		t.Errorf("Mode = %#o, want read-only", created.Attributes.Mode)
	}
	if created.ManagedDirectory {
		t.Error("ManagedDirectory = true, want false")
	}
	if created.OriginScrapeID != nil {
		t.Errorf("OriginScrapeID = %v, want nil", created.OriginScrapeID)
	}

	movies, err := repository.Lookup(context.Background(), libraryRecord.RootNodeID, "Movies")
	if err != nil {
		t.Fatalf("Lookup Movies: %v", err)
	}
	dune, err := repository.Lookup(
		context.Background(),
		libraryRecord.RootNodeID,
		"Movies/Dune (2021)",
	)
	if err != nil {
		t.Fatalf("Lookup Dune directory: %v", err)
	}
	for _, directory := range []namespace.Node{movies, dune} {
		if directory.Kind != namespace.Directory || !directory.ManagedDirectory {
			t.Errorf("managed directory = %#v", directory)
		}
		if directory.Attributes != (namespace.Attributes{
			Mode: libraryRecord.DirectoryMode,
			UID:  libraryRecord.UID,
			GID:  libraryRecord.GID,
		}) {
			t.Errorf("directory attributes = %#v", directory.Attributes)
		}
	}

	assertNamespaceState(t, opened, 2, 2)
	assertProjectionLog(t, opened, "media:/Movies/Dune (2021)/Dune.mkv")
	assertPlaceholderScrapesEmpty(t, opened)
	assertSourceUnchanged(t, sourcePath, sourceContent, before)
}

func TestServiceCreateProjectionConflictLeavesNamespaceAndSourceUnchanged(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	createTestLibrary(t, repository)
	service := NewService(repository)

	firstSource := filepath.Join(t.TempDir(), "first.mkv")
	if err := os.WriteFile(firstSource, []byte("first"), 0o600); err != nil {
		t.Fatalf("WriteFile first source: %v", err)
	}
	if _, err := service.Create(context.Background(), CreateRequest{
		SourcePath: firstSource,
		Target:     "media:/Movies/Dune.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	secondSource := filepath.Join(t.TempDir(), "second.mkv")
	secondContent := []byte("second")
	if err := os.WriteFile(secondSource, secondContent, 0o640); err != nil {
		t.Fatalf("WriteFile second source: %v", err)
	}
	before := statSource(t, secondSource)
	beforeNodes := countNodes(t, opened)

	_, err := service.Create(context.Background(), CreateRequest{
		SourcePath: secondSource,
		Target:     "media:/Movies/Dune.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	})
	if !errors.Is(err, ErrTargetConflict) {
		t.Fatalf("Create conflict error = %v, want ErrTargetConflict", err)
	}

	if got := countNodes(t, opened); got != beforeNodes {
		t.Errorf("node count = %d, want %d", got, beforeNodes)
	}
	assertNamespaceState(t, opened, 2, 2)
	assertPlaceholderScrapesEmpty(t, opened)
	assertSourceUnchanged(t, secondSource, secondContent, before)
}

func TestServiceCreateRejectsPreparedCandidateAfterLibraryRename(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createTestLibrary(t, repository)
	service := NewService(repository)
	ctx := context.Background()
	sourcePath := regularSource(t)
	request := CreateRequest{
		SourcePath: sourcePath,
		Target:     "media:/Movies/Dune.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}
	candidate, err := service.Prepare(ctx, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := library.NewService(repository).Rename(ctx, library.RenameRequest{
		Name:    "media",
		NewName: "movies",
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Rename Library: %v", err)
	}

	if _, err := service.createPrepared(ctx, request, candidate); !errors.Is(
		err,
		namespace.ErrConflict,
	) {
		t.Fatalf("createPrepared error = %v, want ErrConflict", err)
	}
	if _, err := repository.Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Movies/Dune.mkv",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Fatalf("Lookup stale Projection error = %v, want ErrNotFound", err)
	}
	assertNamespaceState(t, opened, 2, 2)
	assertPlaceholderScrapesEmpty(t, opened)
}

func TestServiceCreateProjectionRollsBackAfterNamespaceMutationFailure(t *testing.T) {
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	libraryRecord := createTestLibrary(t, repository)
	service := NewService(repository)

	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(context.Background(), `
			CREATE TEMP TRIGGER fail_projection_log
			BEFORE INSERT ON operation_log
			WHEN NEW.operation = 'projection.create'
			BEGIN
				SELECT RAISE(ABORT, 'forced projection log failure');
			END
		`); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	sourcePath := filepath.Join(t.TempDir(), "source.mkv")
	sourceContent := []byte("source")
	if err := os.WriteFile(sourcePath, sourceContent, 0o640); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	before := statSource(t, sourcePath)

	_, err := service.Create(context.Background(), CreateRequest{
		SourcePath: sourcePath,
		Target:     "media:/Movies/Dune/Dune.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err == nil || !strings.Contains(err.Error(), "forced projection log failure") {
		t.Fatalf("Create error = %v, want forced operation-log failure", err)
	}

	if _, err := repository.Lookup(
		context.Background(),
		libraryRecord.RootNodeID,
		"Movies",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Fatalf("Lookup rolled-back Movies error = %v, want ErrNotFound", err)
	}
	assertNamespaceState(t, opened, 1, 1)
	assertPlaceholderScrapesEmpty(t, opened)
	assertSourceUnchanged(t, sourcePath, sourceContent, before)
}

func TestServiceCreateProjectionRejectsInvalidInputsBeforeMutation(t *testing.T) {
	tests := map[string]struct {
		source func(*testing.T) string
		target string
		want   error
	}{
		"malformed target": {
			source: regularSource,
			target: "media:Movies/Dune.mkv",
			want:   ErrInvalidPath,
		},
		"generic invalid path": {
			source: regularSource,
			target: "media:/Movies//Dune.mkv",
			want:   ErrInvalidPath,
		},
		"Profile unsafe path": {
			source: regularSource,
			target: "media:/Movies/Dune: Part One.mkv",
			want:   ErrInvalidPath,
		},
		"unsafe source path": {
			source: unsafeSource,
			target: "media:/Movies/Dune.mkv",
			want:   ErrUnsafeSource,
		},
		"missing source": {
			source: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "missing.mkv")
			},
			target: "media:/Movies/Dune.mkv",
			want:   ErrSourceNotFound,
		},
		"non-regular source": {
			source: func(t *testing.T) string {
				return t.TempDir()
			},
			target: "media:/Movies/Dune.mkv",
			want:   ErrSourceNotRegular,
		},
		"Unix domain socket source": {
			source: unixSocketSource,
			target: "media:/Movies/Dune.mkv",
			want:   ErrSourceNotRegular,
		},
		"missing Library": {
			source: regularSource,
			target: "missing:/Movies/Dune.mkv",
			want:   ErrLibraryNotFound,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			opened := openTestDatabase(t)
			repository := namespace.NewRepository(opened)
			createTestLibrary(t, repository)
			sourcePath := test.source(t)
			beforeNodes := countNodes(t, opened)

			_, err := NewService(repository).Create(context.Background(), CreateRequest{
				SourcePath: sourcePath,
				Target:     test.target,
				Actor:      namespace.Actor{Kind: namespace.ActorSystem},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("Create error = %v, want %v", err, test.want)
			}
			if got := countNodes(t, opened); got != beforeNodes {
				t.Errorf("node count = %d, want %d", got, beforeNodes)
			}
			assertNamespaceState(t, opened, 1, 1)
			assertPlaceholderScrapesEmpty(t, opened)
		})
	}
}

func TestServiceCreateProjectionPersistsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	ctx := context.Background()

	firstDatabase, err := database.OpenWithOptions(ctx, databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	firstRepository := namespace.NewRepository(firstDatabase)
	libraryRecord := createTestLibrary(t, firstRepository)
	sourcePath := filepath.Join(root, "source.mkv")
	if err := os.WriteFile(sourcePath, []byte("persistent"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	created, err := NewService(firstRepository).Create(ctx, CreateRequest{
		SourcePath: sourcePath,
		Target:     "media:/Movies/Persistent.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
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

	reopened, err := namespace.NewRepository(secondDatabase).Lookup(
		ctx,
		libraryRecord.RootNodeID,
		"Movies/Persistent.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup after restart: %v", err)
	}
	if reopened.ID != created.ID {
		t.Errorf("ID after restart = %d, want %d", reopened.ID, created.ID)
	}
	if reopened.Source == nil || created.Source == nil || *reopened.Source != *created.Source {
		t.Errorf("Source after restart = %#v, want %#v", reopened.Source, created.Source)
	}
	if reopened.OriginScrapeID != nil {
		t.Errorf("OriginScrapeID after restart = %v, want nil", reopened.OriginScrapeID)
	}
	assertNamespaceState(t, secondDatabase, 2, 2)
	assertPlaceholderScrapesEmpty(t, secondDatabase)
}

func TestValidateProfilePath(t *testing.T) {
	for _, profileName := range []string{"jellyfin", "plex"} {
		if err := validateProfilePath(profileName, 1, []string{"Movies", "Dune.mkv"}); err != nil {
			t.Errorf("validateProfilePath(%q): %v", profileName, err)
		}
		if err := validateProfilePath(
			profileName,
			1,
			[]string{"Movies", "Dune\n2021.mkv"},
		); !errors.Is(err, ErrInvalidPath) {
			t.Errorf(
				"validateProfilePath(%q) control-character error = %v, want ErrInvalidPath",
				profileName,
				err,
			)
		}
	}
	if err := validateProfilePath(
		"jellyfin",
		1,
		[]string{"Movies", "Dune: Part One.mkv"},
	); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Jellyfin reserved-character error = %v, want ErrInvalidPath", err)
	}
	if err := validateProfilePath(
		"plex",
		1,
		[]string{"Movies", "Dune: Part One.mkv"},
	); err != nil {
		t.Errorf("Plex path with colon: %v", err)
	}
	if err := validateProfilePath(
		"missing",
		1,
		[]string{"Movies", "Dune.mkv"},
	); !errors.Is(err, namespace.ErrProfileNotFound) {
		t.Errorf("unavailable Profile error = %v, want ErrProfileNotFound", err)
	}
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

func createTestLibrary(t *testing.T, repository *namespace.Repository) namespace.Library {
	t.Helper()

	created, err := library.NewService(repository).Create(context.Background(), library.CreateRequest{
		Name:    "media",
		Profile: library.ProfileIdentifier{Name: "jellyfin", Version: 1},
		Actor:   namespace.Actor{Kind: namespace.ActorSystem},
		UID:     1234,
		GID:     5678,
		Modes: library.Modes{
			Root:       0o751,
			Directory:  0o750,
			Projection: 0o440,
			Overlay:    0o640,
		},
	})
	if err != nil {
		t.Fatalf("Create Library: %v", err)
	}
	return created
}

func regularSource(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	return path
}

func unsafeSource(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	path := filepath.Join(root, "source.mkv")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("Mkdir child: %v", err)
	}
	return child + "/../source.mkv"
}

func unixSocketSource(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "source.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen Unix socket: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("Close Unix socket: %v", err)
		}
	})
	return path
}

func statSource(t *testing.T, path string) unix.Stat_t {
	t.Helper()

	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatalf("Stat %q: %v", path, err)
	}
	return stat
}

func assertSourceUnchanged(
	t *testing.T,
	path string,
	content []byte,
	before unix.Stat_t,
) {
	t.Helper()

	after := statSource(t, path)
	if after.Dev != before.Dev ||
		after.Ino != before.Ino ||
		after.Mode != before.Mode ||
		after.Size != before.Size ||
		after.Atim != before.Atim ||
		after.Mtim != before.Mtim ||
		after.Ctim != before.Ctim {
		t.Errorf("source metadata changed: before=%#v after=%#v", before, after)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile source: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("source content = %q, want %q", got, content)
	}
}

func countNodes(t *testing.T, opened *database.DB) int64 {
	t.Helper()

	var count int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM nodes",
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	return count
}

func assertNamespaceState(t *testing.T, opened *database.DB, wantVersion, wantLogs int64) {
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

func assertProjectionLog(t *testing.T, opened *database.DB, target string) {
	t.Helper()

	var operation string
	var scrapeID sql.NullInt64
	var version int64
	var detailsJSON string
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(context.Background(), `
			SELECT operation, scrape_id, namespace_version, details_json
			FROM operation_log
			ORDER BY id DESC
			LIMIT 1
		`).Scan(&operation, &scrapeID, &version, &detailsJSON)
	}); err != nil {
		t.Fatalf("read Projection operation log: %v", err)
	}
	if operation != "projection.create" {
		t.Errorf("operation = %q, want projection.create", operation)
	}
	if scrapeID.Valid {
		t.Errorf("scrape_id = %d, want NULL", scrapeID.Int64)
	}
	if version != 2 {
		t.Errorf("namespace_version = %d, want 2", version)
	}

	var details namespace.OperationDetails
	if err := json.Unmarshal([]byte(detailsJSON), &details); err != nil {
		t.Fatalf("Unmarshal operation details: %v", err)
	}
	if len(details.Paths) != 1 ||
		details.Paths[0] != target ||
		len(details.NodeKinds) != 1 ||
		details.NodeKinds[0] != namespace.ProjectionFile {
		t.Errorf("operation details = %#v", details)
	}
}

func assertPlaceholderScrapesEmpty(t *testing.T, opened *database.DB) {
	t.Helper()

	var count int64
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM scrapes",
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count scrapes: %v", err)
	}
	if count != 0 {
		t.Errorf("scrapes count = %d, want 0", count)
	}
}
