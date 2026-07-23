package control

import (
	"context"
	"errors"
	"io/fs"
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
		serverDone <- Serve(
			ctx,
			socketPath,
			0o660,
			projectionbatch.NewService(repository),
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
		serverDone <- Serve(
			ctx,
			socketPath,
			0o600,
			projectionbatch.NewService(namespace.NewRepository(opened)),
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
