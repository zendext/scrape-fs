package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestProjectionClientServerCreateAndListRoundTrip(t *testing.T) {
	root := controlSocketTempDir(t)
	opened := openProjectionControlDatabase(t, root)
	defer opened.Close()
	repository := namespace.NewRepository(opened)
	createProjectionControlLibrary(t, repository, "media")

	socketPath := filepath.Join(root, "control.sock")
	cancel, done := startProjectionControlServer(t, socketPath, repository, readPeerIdentity)
	defer stopProjectionControlServer(t, cancel, done)

	source := filepath.Join(root, "Dune.mkv")
	content := []byte("video")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	client := NewClient(socketPath)
	created, err := client.CreateProjection(
		context.Background(),
		projection.CreateRequest{
			SourcePath: source,
			Target:     "media:/Movies/Dune.mkv",
		},
	)
	if err != nil {
		t.Fatalf("CreateProjection: %v", err)
	}
	if created.Schema != ProjectionResultSchema ||
		created.Projection.ID <= 0 ||
		created.Projection.Library != "media" ||
		created.Projection.Target != "media:/Movies/Dune.mkv" ||
		created.Projection.Source.Path != source ||
		created.Projection.Source.Size != int64(len(content)) {
		t.Errorf("CreateProjection result = %#v", created)
	}

	listed, err := client.ListProjections(context.Background(), "")
	if err != nil {
		t.Fatalf("ListProjections: %v", err)
	}
	if listed.Schema != ProjectionListSchema ||
		len(listed.Projections) != 1 ||
		listed.Projections[0] != created.Projection {
		t.Errorf("ListProjections result = %#v, created = %#v", listed, created)
	}
	scoped, err := client.ListProjections(context.Background(), "media")
	if err != nil {
		t.Fatalf("ListProjections(media): %v", err)
	}
	if len(scoped.Projections) != 1 || scoped.Projections[0] != created.Projection {
		t.Errorf("ListProjections(media) = %#v", scoped)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != string(content) {
		t.Errorf("source after create = %q, %v", got, err)
	}
}

func TestProjectionControlReturnsStableDomainErrors(t *testing.T) {
	root := controlSocketTempDir(t)
	opened := openProjectionControlDatabase(t, root)
	defer opened.Close()
	repository := namespace.NewRepository(opened)
	createProjectionControlLibrary(t, repository, "media")

	socketPath := filepath.Join(root, "control.sock")
	cancel, done := startProjectionControlServer(t, socketPath, repository, readPeerIdentity)
	defer stopProjectionControlServer(t, cancel, done)
	client := NewClient(socketPath)

	source := filepath.Join(root, "source.mkv")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	if _, err := client.CreateProjection(
		context.Background(),
		projection.CreateRequest{
			SourcePath: source,
			Target:     "media:/Movies/source.mkv",
		},
	); err != nil {
		t.Fatalf("initial CreateProjection: %v", err)
	}

	tests := []struct {
		name   string
		source string
		target string
		code   string
	}{
		{
			name:   "invalid target",
			source: source,
			target: "media:Movies/invalid.mkv",
			code:   "InvalidPath",
		},
		{
			name:   "missing Library",
			source: source,
			target: "missing:/Movies/source.mkv",
			code:   "LibraryNotFound",
		},
		{
			name:   "target conflict",
			source: source,
			target: "media:/Movies/source.mkv",
			code:   "TargetConflict",
		},
		{
			name:   "unsafe source",
			source: root + "/child/../source.mkv",
			target: "media:/Movies/unsafe.mkv",
			code:   "UnsafeSource",
		},
		{
			name:   "missing source",
			source: filepath.Join(root, "missing.mkv"),
			target: "media:/Movies/missing.mkv",
			code:   "SourceNotFound",
		},
		{
			name:   "non-regular source",
			source: root,
			target: "media:/Movies/directory.mkv",
			code:   "SourceNotRegular",
		},
	}
	if err := os.Mkdir(filepath.Join(root, "child"), 0o700); err != nil {
		t.Fatalf("Mkdir child: %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.CreateProjection(context.Background(), projection.CreateRequest{
				SourcePath: test.source,
				Target:     test.target,
			})
			var remote *RemoteError
			if !errors.As(err, &remote) {
				t.Fatalf("CreateProjection error = %v, want RemoteError", err)
			}
			if remote.Code != test.code {
				t.Errorf("RemoteError code = %q, want %q", remote.Code, test.code)
			}
		})
	}
}

func TestProjectionControlRejectsUnauthorizedCreateBeforeServiceCall(t *testing.T) {
	projectionService := &countingProjectionService{}
	socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
	controlGroupID := uint32(os.Getegid())
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		serverDone <- serveWithOptions(
			ctx,
			socketPath,
			0o660,
			staticBatchService{},
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: 42,
					controlGroupID:   controlGroupID,
				},
				readPeerIdentity: func(*net.UnixConn) (peerIdentity, error) {
					return peerIdentity{
						UID: 1000,
						GID: differentUint32(controlGroupID),
					}, nil
				},
				ready:             ready,
				projectionService: projectionService,
			},
		)
	}()
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("serveWithOptions: %v", err)
		}
	}()
	waitForServerReady(t, ready)

	_, err := NewClient(socketPath).CreateProjection(
		context.Background(),
		projection.CreateRequest{
			SourcePath: "/must/not/be/opened",
			Target:     "media:/Movies/source.mkv",
		},
	)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "PermissionDenied" {
		t.Fatalf("CreateProjection error = %#v, want PermissionDenied", err)
	}
	if projectionService.calls != 0 {
		t.Errorf("Projection service calls = %d, want 0", projectionService.calls)
	}
}

func TestProjectionListPersistsAcrossControlServerRestart(t *testing.T) {
	root := controlSocketTempDir(t)
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	socketPath := filepath.Join(root, "control.sock")

	first, err := database.OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	firstRepository := namespace.NewRepository(first)
	createProjectionControlLibrary(t, firstRepository, "media")
	source := filepath.Join(root, "persistent.mkv")
	if err := os.WriteFile(source, []byte("persistent"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	cancel, done := startProjectionControlServer(t, socketPath, firstRepository, readPeerIdentity)
	created, err := NewClient(socketPath).CreateProjection(
		context.Background(),
		projection.CreateRequest{
			SourcePath: source,
			Target:     "media:/Movies/Persistent.mkv",
		},
	)
	if err != nil {
		t.Fatalf("CreateProjection: %v", err)
	}
	stopProjectionControlServer(t, cancel, done)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := database.OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer second.Close()
	cancel, done = startProjectionControlServer(
		t,
		socketPath,
		namespace.NewRepository(second),
		readPeerIdentity,
	)
	defer stopProjectionControlServer(t, cancel, done)

	listed, err := NewClient(socketPath).ListProjections(context.Background(), "media")
	if err != nil {
		t.Fatalf("ListProjections after restart: %v", err)
	}
	if len(listed.Projections) != 1 || listed.Projections[0] != created.Projection {
		t.Errorf("ListProjections after restart = %#v, created = %#v", listed, created)
	}
}

type countingProjectionService struct {
	calls int
}

func (service *countingProjectionService) Create(
	context.Context,
	projection.CreateRequest,
) (namespace.Node, error) {
	service.calls++
	return namespace.Node{}, nil
}

func (service *countingProjectionService) List(
	context.Context,
	projection.ListRequest,
) ([]namespace.Projection, error) {
	service.calls++
	return nil, nil
}

func openProjectionControlDatabase(t *testing.T, root string) *database.DB {
	t.Helper()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	return opened
}

func createProjectionControlLibrary(
	t *testing.T,
	repository *namespace.Repository,
	name string,
) {
	t.Helper()
	if _, err := library.NewService(repository).Create(
		context.Background(),
		library.CreateRequest{
			Name:    name,
			Profile: library.ProfileIdentifier{Name: "jellyfin", Version: 1},
			Actor:   namespace.Actor{Kind: namespace.ActorSystem},
			UID:     uint32(os.Geteuid()),
			GID:     uint32(os.Getegid()),
			Modes:   library.DefaultModes,
		},
	); err != nil {
		t.Fatalf("Create Library: %v", err)
	}
}

func startProjectionControlServer(
	t *testing.T,
	socketPath string,
	repository *namespace.Repository,
	readIdentity peerIdentityReader,
) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- serveWithOptions(
			ctx,
			socketPath,
			0o660,
			projectionbatch.NewService(repository),
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: uint32(os.Geteuid()),
					controlGroupID:   uint32(os.Getegid()),
				},
				readPeerIdentity:  readIdentity,
				ready:             ready,
				libraryService:    library.NewService(repository),
				projectionService: projection.NewService(repository),
			},
		)
	}()
	waitForServerReady(t, ready)
	return cancel, done
}

func stopProjectionControlServer(
	t *testing.T,
	cancel context.CancelFunc,
	done <-chan error,
) {
	t.Helper()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
