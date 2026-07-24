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

func TestProjectionClientServerLifecycleRoundTrip(t *testing.T) {
	root := controlSocketTempDir(t)
	opened := openProjectionControlDatabase(t, root)
	defer opened.Close()
	repository := namespace.NewRepository(opened)
	createProjectionControlLibrary(t, repository, "media")

	socketPath := filepath.Join(root, "control.sock")
	cancel, done := startProjectionControlServer(t, socketPath, repository, readPeerIdentity)
	defer stopProjectionControlServer(t, cancel, done)
	client := NewClient(socketPath)
	oldSource := filepath.Join(root, "old.mkv")
	oldContent := []byte("old source")
	if err := os.WriteFile(oldSource, oldContent, 0o600); err != nil {
		t.Fatalf("WriteFile old source: %v", err)
	}
	created, err := client.CreateProjection(context.Background(), projection.CreateRequest{
		SourcePath: oldSource,
		Target:     "media:/Movies/Dune.mkv",
	})
	if err != nil {
		t.Fatalf("CreateProjection: %v", err)
	}

	moved, err := client.MoveProjection(context.Background(), projection.MoveRequest{
		Target:    "media:/Movies/Dune.mkv",
		NewTarget: "media:/Films/Dune.mkv",
	})
	if err != nil {
		t.Fatalf("MoveProjection: %v", err)
	}
	if moved.Schema != ProjectionResultSchema ||
		moved.Projection.ID != created.Projection.ID ||
		moved.Projection.Target != "media:/Films/Dune.mkv" {
		t.Errorf("MoveProjection result = %#v", moved)
	}

	newSource := filepath.Join(root, "new.mkv")
	newContent := []byte("new source")
	if err := os.WriteFile(newSource, newContent, 0o600); err != nil {
		t.Fatalf("WriteFile new source: %v", err)
	}
	preview, err := client.RebindProjection(context.Background(), projection.RebindRequest{
		Target:     "media:/Films/Dune.mkv",
		SourcePath: newSource,
	})
	if err != nil {
		t.Fatalf("RebindProjection preview: %v", err)
	}
	if preview.Schema != ProjectionRebindResultSchema ||
		preview.Status != ProjectionRebindConfirmationRequired ||
		preview.ConfirmationDigest == "" ||
		preview.Projection.ID != created.Projection.ID ||
		preview.PreviousSource.Path != oldSource ||
		preview.ProposedSource.Path != newSource {
		t.Errorf("RebindProjection preview = %#v", preview)
	}
	applied, err := client.RebindProjection(context.Background(), projection.RebindRequest{
		Target:             "media:/Films/Dune.mkv",
		SourcePath:         newSource,
		ConfirmationDigest: preview.ConfirmationDigest,
	})
	if err != nil {
		t.Fatalf("RebindProjection apply: %v", err)
	}
	if applied.Status != ProjectionRebindApplied ||
		applied.Projection.ID != created.Projection.ID ||
		applied.Projection.Source.Path != newSource {
		t.Errorf("RebindProjection applied = %#v", applied)
	}

	removed, err := client.RemoveProjection(context.Background(), projection.RemoveRequest{
		Target: "media:/Films/Dune.mkv",
	})
	if err != nil {
		t.Fatalf("RemoveProjection: %v", err)
	}
	if removed.Schema != ProjectionResultSchema ||
		removed.Projection.ID != created.Projection.ID ||
		removed.Projection.Target != "media:/Films/Dune.mkv" {
		t.Errorf("RemoveProjection result = %#v", removed)
	}
	listed, err := client.ListProjections(context.Background(), "media")
	if err != nil {
		t.Fatalf("ListProjections: %v", err)
	}
	if len(listed.Projections) != 0 {
		t.Errorf("Projections after remove = %#v, want empty", listed.Projections)
	}
	for path, content := range map[string][]byte{
		oldSource: oldContent,
		newSource: newContent,
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(content) {
			t.Errorf("source %q after lifecycle = %q, %v", path, got, err)
		}
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

	createProjectionControlLibrary(t, repository, "archive")
	if _, err := client.MoveProjection(context.Background(), projection.MoveRequest{
		Target:    "media:/Movies/source.mkv",
		NewTarget: "archive:/Movies/source.mkv",
	}); remoteErrorCode(err) != "CrossLibrary" {
		t.Errorf("cross-Library MoveProjection error = %v, want CrossLibrary", err)
	}
	if _, err := client.RemoveProjection(
		context.Background(),
		projection.RemoveRequest{Target: "media:/Movies/missing.mkv"},
	); remoteErrorCode(err) != "ProjectionNotFound" {
		t.Errorf("missing RemoveProjection error = %v, want ProjectionNotFound", err)
	}

	rebindSource := filepath.Join(root, "rebind.mkv")
	if err := os.WriteFile(rebindSource, []byte("candidate"), 0o600); err != nil {
		t.Fatalf("WriteFile rebind source: %v", err)
	}
	preview, err := client.RebindProjection(context.Background(), projection.RebindRequest{
		Target:     "media:/Movies/source.mkv",
		SourcePath: rebindSource,
	})
	if err != nil {
		t.Fatalf("RebindProjection preview: %v", err)
	}
	if err := os.WriteFile(rebindSource, []byte("changed candidate"), 0o600); err != nil {
		t.Fatalf("change rebind source: %v", err)
	}
	if _, err := client.RebindProjection(context.Background(), projection.RebindRequest{
		Target:             "media:/Movies/source.mkv",
		SourcePath:         rebindSource,
		ConfirmationDigest: preview.ConfirmationDigest,
	}); remoteErrorCode(err) != "ConfirmationMismatch" {
		t.Errorf("stale RebindProjection error = %v, want ConfirmationMismatch", err)
	}
}

func TestProjectionBatchOwnershipErrorCodeIsStable(t *testing.T) {
	t.Parallel()
	if got := classifyError(projection.ErrBatchOwnedProjection); got != "BatchOwnedProjection" {
		t.Errorf("classifyError = %q, want BatchOwnedProjection", got)
	}
}

func TestLibraryRemovalErrorCodesAreStable(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		namespace.ErrNotEmpty:        "NotEmpty",
		namespace.ErrWriteInProgress: "NamespaceWriteInProgress",
	} {
		if got := classifyError(err); got != want {
			t.Errorf("classifyError(%v) = %q, want %q", err, got, want)
		}
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

func TestProjectionControlRejectsUnauthorizedLifecycleBeforeServiceCall(t *testing.T) {
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

	client := NewClient(socketPath)
	calls := []func() error{
		func() error {
			_, err := client.MoveProjection(context.Background(), projection.MoveRequest{
				Target:    "media:/old",
				NewTarget: "media:/new",
			})
			return err
		},
		func() error {
			_, err := client.RemoveProjection(
				context.Background(),
				projection.RemoveRequest{Target: "media:/old"},
			)
			return err
		},
		func() error {
			_, err := client.RebindProjection(context.Background(), projection.RebindRequest{
				Target:     "media:/old",
				SourcePath: "/must/not/be/opened",
			})
			return err
		},
	}
	for _, call := range calls {
		var remote *RemoteError
		if err := call(); !errors.As(err, &remote) || remote.Code != "PermissionDenied" {
			t.Fatalf("lifecycle error = %#v, want PermissionDenied", err)
		}
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

func (service *countingProjectionService) Move(
	context.Context,
	projection.MoveRequest,
) (namespace.Projection, error) {
	service.calls++
	return namespace.Projection{}, nil
}

func (service *countingProjectionService) Remove(
	context.Context,
	projection.RemoveRequest,
) (namespace.Projection, error) {
	service.calls++
	return namespace.Projection{}, nil
}

func (service *countingProjectionService) Rebind(
	context.Context,
	projection.RebindRequest,
) (projection.RebindResult, error) {
	service.calls++
	return projection.RebindResult{}, nil
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

func remoteErrorCode(err error) string {
	var remote *RemoteError
	if !errors.As(err, &remote) {
		return ""
	}
	return remote.Code
}
