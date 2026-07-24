package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zendext/scrape-fs/internal/objectstore"
)

func TestGarbageCollectClientServerRoundTrip(t *testing.T) {
	socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
	service := &recordingGCService{
		result: objectstore.GCResult{
			Schema:              objectstore.GCResultSchema,
			Examined:            4,
			DeletedLive:         1,
			DeletedStaging:      1,
			RetainedReferenced:  1,
			RetainedGracePeriod: 1,
			Errors:              []objectstore.GCError{},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serveWithOptions(
			ctx,
			socketPath,
			0o660,
			staticBatchService{},
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: uint32(os.Geteuid()),
					controlGroupID:   uint32(os.Getegid()),
				},
				readPeerIdentity: readPeerIdentity,
				ready:            ready,
				gcService:        service,
			},
		)
	}()
	waitForServerReady(t, ready)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	result, err := NewClient(socketPath).GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if result.Schema != objectstore.GCResultSchema ||
		result.Examined != 4 ||
		result.DeletedLive != 1 ||
		result.DeletedStaging != 1 ||
		result.RetainedReferenced != 1 ||
		result.RetainedGracePeriod != 1 ||
		len(result.Errors) != 0 {
		t.Errorf("GC result = %#v", result)
	}
	if service.Calls() != 1 {
		t.Errorf("GC calls = %d, want 1", service.Calls())
	}
}

func TestGarbageCollectRejectsControlUserBeforeService(t *testing.T) {
	socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
	service := &recordingGCService{}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serveWithOptions(
			ctx,
			socketPath,
			0o660,
			staticBatchService{},
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: uint32(os.Geteuid()),
					controlGroupID:   uint32(os.Getegid()),
				},
				readPeerIdentity: func(*net.UnixConn) (peerIdentity, error) {
					return peerIdentity{
						UID: differentUint32(uint32(os.Geteuid())),
						GID: uint32(os.Getegid()),
					}, nil
				},
				ready:     ready,
				gcService: service,
			},
		)
	}()
	waitForServerReady(t, ready)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	_, err := NewClient(socketPath).GarbageCollect(context.Background())
	if !errors.Is(err, ErrPermissionDenied) {
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Code != "PermissionDenied" {
			t.Fatalf("GarbageCollect error = %v, want PermissionDenied", err)
		}
	}
	if service.Calls() != 0 {
		t.Errorf("GC calls = %d, want 0", service.Calls())
	}
}

type recordingGCService struct {
	mu     sync.Mutex
	calls  int
	result objectstore.GCResult
}

func (service *recordingGCService) GarbageCollect(
	context.Context,
) (objectstore.GCResult, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.calls++
	return service.result, nil
}

func (service *recordingGCService) Calls() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.calls
}
