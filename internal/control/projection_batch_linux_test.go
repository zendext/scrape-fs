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
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestProjectionBatchClientServerRoundTrip(t *testing.T) {
	root := t.TempDir()
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
	go func() {
		serverDone <- serveWithLimits(
			ctx,
			socketPath,
			0o660,
			projectionbatch.NewService(repository),
			defaultServerLimits(),
		)
	}()
	waitForSocket(t, socketPath)
	t.Cleanup(func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

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

func TestProjectionBatchClientReturnsMachineReadableRemoteError(t *testing.T) {
	root := t.TempDir()
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
	go func() {
		serverDone <- serveWithLimits(
			ctx,
			socketPath,
			0o600,
			projectionbatch.NewService(namespace.NewRepository(opened)),
			defaultServerLimits(),
		)
	}()
	waitForSocket(t, socketPath)
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

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
	root := t.TempDir()
	socketPath := filepath.Join(root, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveWithLimits(
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
		)
	}()
	waitForSocket(t, socketPath)
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

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

func waitForSocket(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %q was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
