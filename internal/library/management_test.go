package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"github.com/zendext/scrape-fs/internal/projection"
	"golang.org/x/sys/unix"
)

func TestManagementBrowseMoveAndConfirmedRemoval(t *testing.T) {
	ctx := context.Background()
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	store, err := objectstore.OpenWithOptions(filepath.Join(t.TempDir(), "objects"), opened, objectstore.Options{OrphanGracePeriod: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := NewServiceWithObjectReclaimer(repository, store)
	created := createTestLibrary(t, service, "media", "jellyfin")
	actor := namespace.Actor{Kind: namespace.ActorSystem}
	if err := service.CreateDirectory(ctx, "media", "", "Movies", created.RootNodeID, actor); err != nil {
		t.Fatal(err)
	}
	directory, err := service.Browse(ctx, "media", "Movies")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(source, []byte("source stays unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := projection.NewService(repository).Create(ctx, projection.CreateRequest{SourcePath: source, Target: "media:/Movies/film.mkv", Actor: actor}); err != nil {
		t.Fatal(err)
	}
	handle, overlay, err := store.Create(ctx, namespace.Operation{
		Name: "test.overlay", Actor: actor,
		Details: namespace.OperationDetails{Paths: []string{"media:/Movies/movie.nfo"}, NodeKinds: []namespace.NodeKind{namespace.OverlayFile}},
	}, namespace.CreateOverlay{ParentID: directory.ID, Name: "movie.nfo", Creator: actor, Attributes: namespace.Attributes{Mode: 0o644}}, bytes.NewBufferString("metadata"))
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	listing, err := service.Browse(ctx, "media", "Movies")
	if err != nil || len(listing.Entries) != 2 {
		t.Fatalf("listing = %+v, %v", listing, err)
	}
	if listing.Entries[0].Source != source || !listing.Entries[0].CanRebind || listing.Entries[1].Size != 8 {
		t.Fatalf("unexpected metadata: %+v", listing.Entries)
	}
	preview, err := service.PreviewRemoval(ctx, "media", "Movies")
	if err != nil || preview.Mappings != 1 || preview.Files != 1 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	writer, err := store.OpenObject(ctx, *overlay.ObjectID, unix.O_WRONLY)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte("updated metadata has a different size"), 0); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if err := service.RemoveTree(ctx, "media", "Movies", preview.Digest, actor); !errors.Is(err, namespace.ErrPreviewChanged) {
		t.Fatalf("stale content preview: %v", err)
	}
	if err := service.MoveDirectory(ctx, "media", "Movies", "Movies/Child", directory.ID, actor); err == nil {
		t.Fatal("allowed a move into the same subtree")
	}
	if err := service.MoveDirectory(ctx, "media", "Movies", "Films", directory.ID, actor); err != nil {
		t.Fatal(err)
	}
	listing, err = service.Browse(ctx, "media", "Films")
	if err != nil || listing.ID != directory.ID || listing.Entries[0].Source != source {
		t.Fatalf("moved listing = %+v, %v", listing, err)
	}
	preview, err = service.PreviewRemoval(ctx, "media", "Films")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.CreateDirectory(ctx, "media", "Films", "New", directory.ID, actor); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveTree(ctx, "media", "Films", preview.Digest, actor); !errors.Is(err, namespace.ErrPreviewChanged) {
		t.Fatalf("stale namespace preview: %v", err)
	}
	preview, _ = service.PreviewRemoval(ctx, "media", "Films")
	if err := service.RemoveTree(ctx, "media", "Films", preview.Digest, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Node(ctx, directory.ID); !errors.Is(err, namespace.ErrNotFound) {
		t.Fatalf("directory retained after removal: %v", err)
	}
	content, err := os.ReadFile(source)
	if err != nil || string(content) != "source stays unchanged" {
		t.Fatalf("source changed: %q, %v", content, err)
	}
}

func TestManagementRemovalRollsBackAndProtectsLibraryRoot(t *testing.T) {
	ctx := context.Background()
	opened := openTestDatabase(t)
	repository := namespace.NewRepository(opened)
	service := NewService(repository)
	created := createTestLibrary(t, service, "media", "jellyfin")
	actor := namespace.Actor{Kind: namespace.ActorSystem}
	if err := service.CreateDirectory(ctx, "media", "", "Folder", created.RootNodeID, actor); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewRemoval(ctx, "media", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveTree(ctx, "media", "", "", actor); !errors.Is(err, namespace.ErrPreviewChanged) {
		t.Fatalf("missing confirmation accepted: %v", err)
	}
	injectOperationFailure(t, opened, "library.remove")
	if err := service.RemoveTree(ctx, "media", "", preview.Digest, actor); err == nil {
		t.Fatal("removal ignored failed audit insert")
	}
	if _, err := service.Get(ctx, "media"); err != nil {
		t.Fatalf("library retirement was not rolled back: %v", err)
	}
	listing, err := service.Browse(ctx, "media", "")
	if err != nil || len(listing.Entries) != 1 {
		t.Fatalf("tree removal was not rolled back: %+v, %v", listing, err)
	}
	if err := service.MoveDirectory(ctx, "media", "", "Renamed", created.RootNodeID, actor); !errors.Is(err, namespace.ErrProtected) {
		t.Fatalf("ordinary directory operation changed the library root: %v", err)
	}
}
