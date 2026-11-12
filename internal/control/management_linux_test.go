package control

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zendext/scrape-fs/internal/diagnostics"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestDirectoryManagementRPCEnforcesRolesAndFreshConfirmation(t *testing.T) {
	root := controlSocketTempDir(t)
	opened := openProjectionControlDatabase(t, root)
	repository := namespace.NewRepository(opened)
	var caller atomic.Uint32
	caller.Store(42)
	server, err := startWithOptions(filepath.Join(root, "control.sock"), 0o600, projectionbatch.NewService(repository), serverOptions{
		limits: defaultServerLimits(), security: serverSecurity{daemonUID: uint32(os.Geteuid()), administratorUID: 42, controlGroupID: uint32(os.Getegid())},
		readPeerIdentity: func(*net.UnixConn) (peerIdentity, error) {
			return peerIdentity{UID: caller.Load(), GID: uint32(os.Getegid())}, nil
		},
		libraryService: library.NewService(repository), diagnosticsService: &recordingDiagnosticsService{status: diagnostics.StatusResult{Schema: diagnostics.StatusSchema}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := NewClient(filepath.Join(root, "control.sock"))
	ctx := context.Background()
	created, err := client.CreateLibrary(ctx, "media", library.ProfileIdentifier{Name: "jellyfin", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CreateDirectory(ctx, "media", "", "Films", created.Library.RootNodeID); err != nil {
		t.Fatal(err)
	}
	listing, err := client.Browse(ctx, "media", "Films")
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(ctx)
	if err != nil || !status.CanAdmin {
		t.Fatalf("administrator status=%+v, %v", status, err)
	}
	doctor, err := client.Doctor(ctx)
	if err != nil || !doctor.Status.CanAdmin {
		t.Fatalf("administrator doctor role=%+v %v", doctor, err)
	}
	caller.Store(1000)
	if _, err := client.Browse(ctx, "media", ""); err != nil {
		t.Fatal(err)
	}
	preview, err := client.PreviewRemoval(ctx, "media", "Films")
	if err != nil {
		t.Fatal(err)
	}
	status, err = client.Status(ctx)
	if err != nil || status.CanAdmin {
		t.Fatalf("control status=%+v, %v", status, err)
	}
	doctor, err = client.Doctor(ctx)
	if err != nil || doctor.Status.CanAdmin {
		t.Fatalf("control doctor role=%+v %v", doctor, err)
	}
	for _, mutation := range []func() error{
		func() error { return client.CreateDirectory(ctx, "media", "Films", "Child", listing.ID) },
		func() error { return client.MoveDirectory(ctx, "media", "Films", "Movies", listing.ID) },
		func() error { return client.RemoveTree(ctx, "media", "Films", preview.Digest) },
	} {
		if err := mutation(); err == nil || failureCode(err) != "PermissionDenied" {
			t.Fatalf("control mutation error=%v", err)
		}
	}
	caller.Store(42)
	if err := client.CreateDirectory(ctx, "media", "Films", "Child", listing.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveTree(ctx, "media", "Films", preview.Digest); failureCode(err) != "ConfirmationMismatch" {
		t.Fatalf("stale preview error=%v", err)
	}
	if err := client.MoveDirectory(ctx, "media", "Films", "Movies", listing.ID); err != nil {
		t.Fatal(err)
	}
	preview, err = client.PreviewRemoval(ctx, "media", "Movies")
	if err != nil || preview.Directories != 1 {
		t.Fatalf("preview=%+v %v", preview, err)
	}
	if err := client.RemoveTree(ctx, "media", "Movies", preview.Digest); err != nil {
		t.Fatal(err)
	}
	listing, err = client.Browse(ctx, "media", "")
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("listing=%+v %v", listing, err)
	}
}

func failureCode(err error) string {
	if remote, ok := err.(*RemoteError); ok {
		return remote.Code
	}
	return ""
}

func TestDirectoryManagementLookupErrorCodesAreStable(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		namespace.ErrNotFound:     "NotFound",
		namespace.ErrNotDirectory: "NotDirectory",
	} {
		if got := classifyError(err); got != want {
			t.Errorf("classifyError(%v) = %q, want %q", err, got, want)
		}
	}
}
