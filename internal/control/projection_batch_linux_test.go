package control

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestProjectionBatchClientServerRoundTrip(t *testing.T) {
	root := controlSocketTempDir(t)
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()
	repository := namespace.NewRepository(opened)
	if _, err := library.NewService(repository).Create(
		context.Background(),
		library.CreateRequest{
			Name:    "media",
			Profile: library.ProfileIdentifier{Name: "jellyfin", Version: 1},
			Actor:   namespace.Actor{Kind: namespace.ActorSystem},
			UID:     uint32(os.Geteuid()),
			GID:     uint32(os.Getegid()),
			Modes:   library.DefaultModes,
		},
	); err != nil {
		t.Fatalf("Create Library: %v", err)
	}

	socketPath := filepath.Join(root, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		serverDone <- serveWithTestOptions(
			ctx,
			socketPath,
			0o660,
			projectionbatch.NewService(repository),
			defaultServerLimits(),
			ready,
		)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	waitForServerReady(t, ready)

	source := filepath.Join(root, "Dune.mkv")
	if err := os.WriteFile(source, []byte("video"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	data := []byte(`{
		"schema":"scrapefs.projection-batch/v1",
		"library":"media",
		"entries":[{"source":"` + source + `","target":"Movies/Dune.mkv"}]
	}`)
	client := NewClient(socketPath)

	validation, err := client.Validate(context.Background(), data)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	applied, err := client.Apply(context.Background(), projectionbatch.ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	listed, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed.Batches) != 1 || listed.Batches[0].BatchID != applied.BatchID {
		t.Errorf("List = %#v", listed)
	}
	if listed.Batches[0].AppliedBy != (projectionbatch.Caller{
		UID: uint32(os.Geteuid()),
		GID: uint32(os.Getegid()),
	}) {
		t.Errorf("AppliedBy = %#v, want peer credentials", listed.Batches[0].AppliedBy)
	}
	shown, err := client.Show(context.Background(), applied.BatchID)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(shown.Entries) != 1 || shown.Entries[0].Source.Path != source {
		t.Errorf("Show = %#v", shown)
	}
	undone, err := client.Undo(context.Background(), projectionbatch.UndoRequest{
		BatchID: applied.BatchID,
	})
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if undone.Status != "undone" {
		t.Errorf("Undo = %#v", undone)
	}

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	if info.Mode().Perm() != fs.FileMode(0o660) {
		t.Errorf("socket mode = %#o, want 0660", info.Mode().Perm())
	}
}

func TestLibraryClientServerRoundTripUsesPeerOwnershipAndDefaultModes(t *testing.T) {
	root := controlSocketTempDir(t)
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()
	repository := namespace.NewRepository(opened)

	socketPath := filepath.Join(root, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		serverDone <- serveWithOptions(
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
				readPeerIdentity: readPeerIdentity,
				ready:            ready,
				libraryService:   library.NewService(repository),
			},
		)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	waitForServerReady(t, ready)

	client := NewClient(socketPath)
	created, err := client.CreateLibrary(
		context.Background(),
		"media",
		library.ProfileIdentifier{Name: "jellyfin", Version: 1},
	)
	if err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	if created.Schema != LibraryResultSchema ||
		created.Library.Name != "media" ||
		created.Library.ProfileName != "jellyfin" ||
		created.Library.ProfileVersion != 1 ||
		created.Library.UID != uint32(os.Geteuid()) ||
		created.Library.GID != uint32(os.Getegid()) ||
		created.Library.RootMode != library.DefaultModes.Root ||
		created.Library.DirectoryMode != library.DefaultModes.Directory ||
		created.Library.ProjectionMode != library.DefaultModes.Projection ||
		created.Library.OverlayMode != library.DefaultModes.Overlay {
		t.Errorf("CreateLibrary result = %#v", created)
	}

	renamed, err := client.RenameLibrary(context.Background(), "media", "movies")
	if err != nil {
		t.Fatalf("RenameLibrary: %v", err)
	}
	if renamed.Schema != LibraryResultSchema ||
		renamed.Library.ID != created.Library.ID ||
		renamed.Library.RootNodeID != created.Library.RootNodeID ||
		renamed.Library.Name != "movies" {
		t.Errorf("RenameLibrary result = %#v, created = %#v", renamed, created)
	}

	profileChanged, err := client.SetLibraryProfile(
		context.Background(),
		"movies",
		library.ProfileIdentifier{Name: "plex", Version: 1},
	)
	if err != nil {
		t.Fatalf("SetLibraryProfile: %v", err)
	}
	if profileChanged.Schema != LibraryResultSchema ||
		profileChanged.Library.ID != created.Library.ID ||
		profileChanged.Library.RootNodeID != created.Library.RootNodeID ||
		profileChanged.Library.ProfileName != "plex" ||
		profileChanged.Library.ProfileVersion != 1 {
		t.Errorf("SetLibraryProfile result = %#v", profileChanged)
	}

	listed, err := client.ListLibraries(context.Background())
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	if listed.Schema != LibraryListSchema ||
		len(listed.Libraries) != 1 ||
		listed.Libraries[0] != profileChanged.Library {
		t.Errorf("ListLibraries result = %#v, changed = %#v", listed, profileChanged)
	}

	if _, err := repository.Update(
		context.Background(),
		namespace.Operation{
			Name:  "test.directory.create",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{"movies/Shows"},
				NodeKinds: []namespace.NodeKind{namespace.Directory},
			},
		},
		func(transaction *namespace.Transaction) error {
			_, err := transaction.CreateNode(context.Background(), namespace.CreateNode{
				ParentID: profileChanged.Library.RootNodeID,
				Name:     "Shows",
				Kind:     namespace.Directory,
				Creator:  namespace.Actor{Kind: namespace.ActorSystem},
				Attributes: namespace.Attributes{
					Mode: 0o775,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create Library child: %v", err)
	}
	if _, err := client.RemoveLibrary(context.Background(), "movies", false); err == nil {
		t.Fatal("ordinary RemoveLibrary succeeded for non-empty Library")
	} else if remote, ok := err.(*RemoteError); !ok || remote.Code != "NotEmpty" {
		t.Fatalf("ordinary RemoveLibrary error = %v, want NotEmpty", err)
	}
	removed, err := client.RemoveLibrary(context.Background(), "movies", true)
	if err != nil {
		t.Fatalf("recursive RemoveLibrary: %v", err)
	}
	if removed.Schema != LibraryResultSchema ||
		removed.Library.ID != created.Library.ID ||
		removed.Library.Status != namespace.LibraryRetired ||
		removed.Library.RootNodeID != 0 {
		t.Errorf("RemoveLibrary result = %#v", removed)
	}
}

func TestLibraryClientReturnsStableDomainErrors(t *testing.T) {
	root := controlSocketTempDir(t)
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()
	repository := namespace.NewRepository(opened)

	socketPath := filepath.Join(root, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		serverDone <- serveWithOptions(
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
				readPeerIdentity: readPeerIdentity,
				ready:            ready,
				libraryService:   library.NewService(repository),
			},
		)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	waitForServerReady(t, ready)

	client := NewClient(socketPath)
	if _, err := client.CreateLibrary(
		context.Background(),
		"media",
		library.ProfileIdentifier{Name: "jellyfin", Version: 1},
	); err != nil {
		t.Fatalf("first CreateLibrary: %v", err)
	}
	if _, err := client.CreateLibrary(
		context.Background(),
		"other",
		library.ProfileIdentifier{Name: "plex", Version: 1},
	); err != nil {
		t.Fatalf("second CreateLibrary: %v", err)
	}
	tests := []struct {
		name string
		call func() error
		code string
	}{
		{
			name: "duplicate create",
			call: func() error {
				_, err := client.CreateLibrary(
					context.Background(),
					"media",
					library.ProfileIdentifier{Name: "jellyfin", Version: 1},
				)
				return err
			},
			code: "LibraryExists",
		},
		{
			name: "missing profile",
			call: func() error {
				_, err := client.SetLibraryProfile(
					context.Background(),
					"media",
					library.ProfileIdentifier{Name: "jellyfin", Version: 2},
				)
				return err
			},
			code: "ProfileNotFound",
		},
		{
			name: "rename conflict",
			call: func() error {
				_, err := client.RenameLibrary(context.Background(), "media", "other")
				return err
			},
			code: "LibraryExists",
		},
		{
			name: "missing Library",
			call: func() error {
				_, err := client.RenameLibrary(context.Background(), "missing", "renamed")
				return err
			},
			code: "LibraryNotFound",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			var remote *RemoteError
			if !errors.As(err, &remote) {
				t.Fatalf("Library mutation error = %v, want RemoteError", err)
			}
			if remote.Code != test.code {
				t.Errorf("RemoteError code = %q, want %q", remote.Code, test.code)
			}
		})
	}
}

func TestLibraryListPersistsAcrossControlServerRestart(t *testing.T) {
	root := controlSocketTempDir(t)
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	databaseOptions := database.Options{
		LockPath: filepath.Join(root, "run", "scrapefsd.lock"),
	}
	socketPath := filepath.Join(root, "control.sock")

	startServer := func(t *testing.T, opened *database.DB) (context.CancelFunc, <-chan error) {
		t.Helper()
		repository := namespace.NewRepository(opened)
		ctx, cancel := context.WithCancel(context.Background())
		serverDone := make(chan error, 1)
		ready := make(chan struct{})
		go func() {
			serverDone <- serveWithOptions(
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
					readPeerIdentity: readPeerIdentity,
					ready:            ready,
					libraryService:   library.NewService(repository),
				},
			)
		}()
		waitForServerReady(t, ready)
		return cancel, serverDone
	}
	stopServer := func(t *testing.T, cancel context.CancelFunc, done <-chan error) {
		t.Helper()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Serve: %v", err)
		}
	}

	first, err := database.OpenWithOptions(
		context.Background(),
		databasePath,
		databaseOptions,
	)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	cancel, done := startServer(t, first)
	if _, err := NewClient(socketPath).CreateLibrary(
		context.Background(),
		"persisted",
		library.ProfileIdentifier{Name: "plex", Version: 1},
	); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	if _, err := NewClient(socketPath).RenameLibrary(
		context.Background(),
		"persisted",
		"renamed",
	); err != nil {
		t.Fatalf("RenameLibrary: %v", err)
	}
	if _, err := NewClient(socketPath).SetLibraryProfile(
		context.Background(),
		"renamed",
		library.ProfileIdentifier{Name: "jellyfin", Version: 1},
	); err != nil {
		t.Fatalf("SetLibraryProfile: %v", err)
	}
	stopServer(t, cancel, done)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := database.OpenWithOptions(
		context.Background(),
		databasePath,
		databaseOptions,
	)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer second.Close()
	cancel, done = startServer(t, second)
	defer stopServer(t, cancel, done)

	listed, err := NewClient(socketPath).ListLibraries(context.Background())
	if err != nil {
		t.Fatalf("ListLibraries after restart: %v", err)
	}
	if len(listed.Libraries) != 1 ||
		listed.Libraries[0].Name != "renamed" ||
		listed.Libraries[0].ProfileName != "jellyfin" {
		t.Errorf("ListLibraries after restart = %#v", listed)
	}
}

func TestProjectionBatchClientReturnsMachineReadableRemoteError(t *testing.T) {
	root := controlSocketTempDir(t)
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer opened.Close()

	socketPath := filepath.Join(root, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		serverDone <- serveWithTestOptions(
			ctx,
			socketPath,
			0o600,
			projectionbatch.NewService(namespace.NewRepository(opened)),
			defaultServerLimits(),
			ready,
		)
	}()
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	waitForServerReady(t, ready)

	_, err = NewClient(socketPath).Validate(context.Background(), []byte(`{}`))
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("Validate error = %v, want RemoteError", err)
	}
	if remote.Code != "InvalidBatch" || !strings.Contains(remote.Error(), "schema") {
		t.Errorf("RemoteError = %#v", remote)
	}
}

func TestServeBoundsIdleAndOversizedConnections(t *testing.T) {
	root := controlSocketTempDir(t)
	socketPath := filepath.Join(root, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		serverDone <- serveWithTestOptions(
			ctx,
			socketPath,
			0o600,
			staticBatchService{},
			serverLimits{
				maxConnections:   2,
				readTimeout:      50 * time.Millisecond,
				writeTimeout:     time.Second,
				maxRequestBytes:  256,
				acceptRetryDelay: time.Millisecond,
			},
			ready,
		)
	}()
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	waitForServerReady(t, ready)

	var idle []net.Conn
	for range 6 {
		connection, err := net.Dial("unix", socketPath)
		if err != nil {
			t.Fatalf("Dial idle connection: %v", err)
		}
		idle = append(idle, connection)
	}
	defer func() {
		for _, connection := range idle {
			connection.Close()
		}
	}()
	time.Sleep(150 * time.Millisecond)

	listed, err := NewClient(socketPath).List(context.Background())
	if err != nil {
		t.Fatalf("List after idle connections: %v", err)
	}
	if listed.Schema != projectionbatch.ListSchema {
		t.Errorf("List schema = %q, want %q", listed.Schema, projectionbatch.ListSchema)
	}
	select {
	case err := <-serverDone:
		t.Fatalf("server exited after idle connections: %v", err)
	default:
	}

	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial oversized request: %v", err)
	}
	if _, err := connection.Write([]byte(strings.Repeat(" ", 300))); err != nil {
		t.Fatalf("write oversized request: %v", err)
	}
	if unixConnection, ok := connection.(*net.UnixConn); ok {
		if err := unixConnection.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite oversized request: %v", err)
		}
	}
	var response response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		t.Fatalf("decode oversized response: %v", err)
	}
	connection.Close()
	if response.Error == nil {
		t.Fatal("oversized request returned no error")
	}

	if _, err := NewClient(socketPath).List(context.Background()); err != nil {
		t.Fatalf("List after oversized request: %v", err)
	}
}

type staticBatchService struct{}

func (staticBatchService) Validate(
	context.Context,
	[]byte,
) (projectionbatch.ValidationResult, error) {
	return projectionbatch.ValidationResult{Schema: projectionbatch.ValidationSchema}, nil
}

func (staticBatchService) Apply(
	context.Context,
	projectionbatch.ApplyRequest,
) (projectionbatch.ApplyResult, error) {
	return projectionbatch.ApplyResult{Schema: projectionbatch.ResultSchema}, nil
}

func (staticBatchService) List(
	context.Context,
) (projectionbatch.ListResult, error) {
	return projectionbatch.ListResult{
		Schema:  projectionbatch.ListSchema,
		Batches: []projectionbatch.BatchSummary{},
	}, nil
}

func (staticBatchService) Show(
	context.Context,
	string,
) (projectionbatch.DetailResult, error) {
	return projectionbatch.DetailResult{Schema: projectionbatch.DetailSchema}, nil
}

func (staticBatchService) Undo(
	context.Context,
	projectionbatch.UndoRequest,
) (projectionbatch.UndoResult, error) {
	return projectionbatch.UndoResult{Schema: projectionbatch.UndoSchema}, nil
}

func serveWithTestOptions(
	ctx context.Context,
	socketPath string,
	mode fs.FileMode,
	service BatchService,
	limits serverLimits,
	ready chan<- struct{},
) error {
	return serveWithOptions(ctx, socketPath, mode, service, serverOptions{
		limits: limits,
		security: serverSecurity{
			daemonUID:        uint32(os.Geteuid()),
			administratorUID: uint32(os.Geteuid()),
			controlGroupID:   uint32(os.Getegid()),
		},
		readPeerIdentity: readPeerIdentity,
		ready:            ready,
	})
}

func waitForServerReady(t *testing.T, ready <-chan struct{}) {
	t.Helper()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("control server did not become ready")
	}
}

func TestServerStopAcceptingDrainsHandlersBeforeSocketClose(t *testing.T) {
	socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
	service := &blockingBatchService{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	ready := make(chan struct{})
	server, err := startWithOptions(
		socketPath,
		0o660,
		service,
		serverOptions{
			limits: defaultServerLimits(),
			security: serverSecurity{
				daemonUID:        uint32(os.Geteuid()),
				administratorUID: uint32(os.Geteuid()),
				controlGroupID:   uint32(os.Getegid()),
			},
			readPeerIdentity: readPeerIdentity,
			ready:            ready,
		},
	)
	if err != nil {
		t.Fatalf("startWithOptions: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-service.release:
		default:
			close(service.release)
		}
		if err := server.Close(); err != nil {
			t.Errorf("server Close: %v", err)
		}
	})
	waitForServerReady(t, ready)

	requestDone := make(chan error, 1)
	go func() {
		_, err := NewClient(socketPath).Validate(
			context.Background(),
			[]byte(`{"schema":"scrapefs.projection-batch/v1"}`),
		)
		requestDone <- err
	}()
	select {
	case <-service.entered:
	case <-time.After(time.Second):
		t.Fatal("control request did not enter service")
	}

	if err := server.StopAccepting(); err != nil {
		t.Fatalf("StopAccepting: %v", err)
	}
	if info, err := os.Lstat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket after StopAccepting = %v/%v, want existing socket", info, err)
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- server.Wait()
	}()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned before handler drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(service.release)
	if err := <-requestDone; err != nil {
		t.Fatalf("Validate: %v", err)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after handler completed")
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after Close error = %v, want not exist", err)
	}
}

type blockingBatchService struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (service *blockingBatchService) Validate(
	context.Context,
	[]byte,
) (projectionbatch.ValidationResult, error) {
	service.once.Do(func() {
		close(service.entered)
	})
	<-service.release
	return projectionbatch.ValidationResult{
		Schema: projectionbatch.ValidationSchema,
	}, nil
}

func (*blockingBatchService) Apply(
	context.Context,
	projectionbatch.ApplyRequest,
) (projectionbatch.ApplyResult, error) {
	return projectionbatch.ApplyResult{}, nil
}

func (*blockingBatchService) List(
	context.Context,
) (projectionbatch.ListResult, error) {
	return projectionbatch.ListResult{}, nil
}

func (*blockingBatchService) Show(
	context.Context,
	string,
) (projectionbatch.DetailResult, error) {
	return projectionbatch.DetailResult{}, nil
}

func (*blockingBatchService) Undo(
	context.Context,
	projectionbatch.UndoRequest,
) (projectionbatch.UndoResult, error) {
	return projectionbatch.UndoResult{}, nil
}

func controlSocketTempDir(t *testing.T) string {
	t.Helper()

	path, err := os.MkdirTemp(os.TempDir(), "scrapefs-control-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Errorf("RemoveAll %q: %v", path, err)
		}
	})
	return path
}
