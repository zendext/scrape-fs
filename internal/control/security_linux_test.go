package control

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestServerSecurityAuthorizesCommandRoles(t *testing.T) {
	t.Parallel()

	security := serverSecurity{
		administratorUID: 42,
		controlGroupID:   84,
	}
	tests := []struct {
		name     string
		command  string
		identity peerIdentity
		wantErr  bool
	}{
		{
			name:     "root administers",
			command:  "library.add",
			identity: peerIdentity{UID: 0},
		},
		{
			name:     "configured administrator administers",
			command:  "gc",
			identity: peerIdentity{UID: 42},
		},
		{
			name:     "administrator uses control command",
			command:  "projection.batch.list",
			identity: peerIdentity{UID: 42},
		},
		{
			name:     "primary control group uses control command",
			command:  "projection.batch.apply",
			identity: peerIdentity{UID: 1000, GID: 84},
		},
		{
			name:     "supplementary control group uses control command",
			command:  "doctor",
			identity: peerIdentity{UID: 1000, GID: 1000, Groups: []uint32{84}},
		},
		{
			name:     "control user cannot administer",
			command:  "library.rename",
			identity: peerIdentity{UID: 1000, GID: 84},
			wantErr:  true,
		},
		{
			name:     "unprivileged user cannot use control command",
			command:  "status",
			identity: peerIdentity{UID: 1000, GID: 1000},
			wantErr:  true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := security.authorize(test.command, test.identity)
			if test.wantErr {
				if !errors.Is(err, ErrPermissionDenied) {
					t.Fatalf("authorize error = %v, want ErrPermissionDenied", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
		})
	}
}

func TestPeerIdentityUsesSocketCredentialsAndSupplementaryGroups(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	defer listener.Close()

	accepted := make(chan *net.UnixConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()

	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}
	defer client.Close()

	var server *net.UnixConn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("AcceptUnix: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("AcceptUnix timed out")
	}
	defer server.Close()

	identity, err := readPeerIdentity(server)
	if err != nil {
		t.Fatalf("readPeerIdentity: %v", err)
	}
	if identity.PID != int32(os.Getpid()) ||
		identity.UID != uint32(os.Geteuid()) ||
		identity.GID != uint32(os.Getegid()) {
		t.Errorf("identity = %#v, want pid=%d uid=%d gid=%d", identity, os.Getpid(), os.Geteuid(), os.Getegid())
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("Getgroups: %v", err)
	}
	for _, group := range groups {
		if !containsGroup(identity.Groups, uint32(group)) {
			t.Errorf("peer groups %v do not contain process group %d", identity.Groups, group)
		}
	}
}

func TestServeRejectsUnauthorizedRequestBeforeDispatch(t *testing.T) {
	service := &countingBatchService{}
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveWithOptions(
			ctx,
			socketPath,
			0o660,
			service,
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: 42,
					controlGroupID:   uint32(os.Getegid()),
				},
				readPeerIdentity: func(*net.UnixConn) (peerIdentity, error) {
					return peerIdentity{
						UID: 1000,
						GID: differentUint32(uint32(os.Getegid())),
					}, nil
				},
			},
		)
	}()
	waitForSocket(t, socketPath)
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("serveWithOptions: %v", err)
		}
	}()

	_, err := NewClient(socketPath).List(context.Background())
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("List error = %v, want RemoteError", err)
	}
	if remote.Code != "PermissionDenied" {
		t.Errorf("RemoteError code = %q, want PermissionDenied", remote.Code)
	}
	if service.Calls() != 0 {
		t.Errorf("service calls = %d, want 0", service.Calls())
	}
}

func TestServeAllowsControlGroupRequest(t *testing.T) {
	service := &countingBatchService{}
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	controlGroupID := uint32(os.Getegid())
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveWithOptions(
			ctx,
			socketPath,
			0o660,
			service,
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: 42,
					controlGroupID:   controlGroupID,
				},
				readPeerIdentity: func(*net.UnixConn) (peerIdentity, error) {
					return peerIdentity{UID: 1000, GID: controlGroupID}, nil
				},
			},
		)
	}()
	waitForSocket(t, socketPath)
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("serveWithOptions: %v", err)
		}
	}()

	listed, err := NewClient(socketPath).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if listed.Schema != projectionbatch.ListSchema {
		t.Errorf("List schema = %q, want %q", listed.Schema, projectionbatch.ListSchema)
	}
	if service.Calls() != 1 {
		t.Errorf("service calls = %d, want 1", service.Calls())
	}
}

func TestServeSetsSocketOwnerGroupAndMode(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveWithOptions(
			ctx,
			socketPath,
			0o640,
			staticBatchService{},
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: uint32(os.Geteuid()),
					controlGroupID:   uint32(os.Getegid()),
				},
				readPeerIdentity: readPeerIdentity,
			},
		)
	}()
	waitForSocket(t, socketPath)
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("serveWithOptions: %v", err)
		}
	}()

	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat socket: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("socket stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		t.Errorf(
			"socket ownership = %d:%d, want %d:%d",
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}
	if info.Mode().Perm() != fs.FileMode(0o640) {
		t.Errorf("socket mode = %#o, want 0640", info.Mode().Perm())
	}
}

func TestPrepareSocketPathRejectsUnsafeOrUnexpectedExistingPaths(t *testing.T) {
	t.Parallel()

	security := serverSecurity{
		daemonUID:      uint32(os.Geteuid()),
		controlGroupID: uint32(os.Getegid()),
	}
	t.Run("symlink parent", func(t *testing.T) {
		root := t.TempDir()
		realParent := filepath.Join(root, "real")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatalf("Mkdir real parent: %v", err)
		}
		linkParent := filepath.Join(root, "link")
		if err := os.Symlink(realParent, linkParent); err != nil {
			t.Fatalf("Symlink parent: %v", err)
		}

		prepared, err := prepareSocketPath(filepath.Join(linkParent, "control.sock"), security)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil {
			t.Fatal("prepareSocketPath succeeded through symlink parent")
		}
	})

	t.Run("symlink socket", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatalf("WriteFile target: %v", err)
		}
		socketPath := filepath.Join(root, "control.sock")
		if err := os.Symlink(target, socketPath); err != nil {
			t.Fatalf("Symlink socket: %v", err)
		}

		prepared, err := prepareSocketPath(socketPath, security)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "not a socket") {
			t.Fatalf("prepareSocketPath error = %v, want not-a-socket error", err)
		}
	})

	t.Run("writable parent", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o770); err != nil {
			t.Fatalf("Chmod parent: %v", err)
		}

		prepared, err := prepareSocketPath(filepath.Join(parent, "control.sock"), security)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "must not be writable") {
			t.Fatalf("prepareSocketPath error = %v, want writable-parent error", err)
		}
	})

	t.Run("unexpected stale socket group", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "control.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			t.Fatalf("ListenUnix stale socket: %v", err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatalf("Close stale socket: %v", err)
		}

		unexpected := security
		unexpected.controlGroupID = differentUint32(uint32(os.Getegid()))
		prepared, err := prepareSocketPath(socketPath, unexpected)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "ownership") {
			t.Fatalf("prepareSocketPath error = %v, want ownership error", err)
		}
		if _, err := os.Lstat(socketPath); err != nil {
			t.Fatalf("unexpected stale socket was removed: %v", err)
		}
	})

	t.Run("expected stale socket", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "control.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			t.Fatalf("ListenUnix stale socket: %v", err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatalf("Close stale socket: %v", err)
		}

		prepared, err := prepareSocketPath(socketPath, security)
		if err != nil {
			t.Fatalf("prepareSocketPath: %v", err)
		}
		defer prepared.Close()
		if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale socket stat error = %v, want not exist", err)
		}
	})
}

func TestResolveGroupIDRejectsInvalidLookupResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		lookup groupLookup
	}{
		{
			name: "missing group",
			lookup: func(string) (string, error) {
				return "", errors.New("not found")
			},
		},
		{
			name: "invalid gid",
			lookup: func(string) (string, error) {
				return "not-a-number", nil
			},
		},
		{
			name: "overflow gid",
			lookup: func(string) (string, error) {
				return strconv.FormatUint(uint64(^uint32(0))+1, 10), nil
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := resolveGroupID(ControlGroupName, test.lookup); err == nil {
				t.Fatal("resolveGroupID succeeded")
			}
		})
	}
}

func TestResolveGroupID(t *testing.T) {
	t.Parallel()

	got, err := resolveGroupID(ControlGroupName, func(name string) (string, error) {
		if name != ControlGroupName {
			t.Errorf("group name = %q, want %q", name, ControlGroupName)
		}
		return "1234", nil
	})
	if err != nil {
		t.Fatalf("resolveGroupID: %v", err)
	}
	if got != 1234 {
		t.Errorf("group ID = %d, want 1234", got)
	}
}

type countingBatchService struct {
	mu    sync.Mutex
	calls int
}

func (service *countingBatchService) Calls() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.calls
}

func (service *countingBatchService) record() {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.calls++
}

func (service *countingBatchService) Validate(
	context.Context,
	[]byte,
) (projectionbatch.ValidationResult, error) {
	service.record()
	return projectionbatch.ValidationResult{Schema: projectionbatch.ValidationSchema}, nil
}

func (service *countingBatchService) Apply(
	context.Context,
	projectionbatch.ApplyRequest,
) (projectionbatch.ApplyResult, error) {
	service.record()
	return projectionbatch.ApplyResult{Schema: projectionbatch.ResultSchema}, nil
}

func (service *countingBatchService) List(
	context.Context,
) (projectionbatch.ListResult, error) {
	service.record()
	return projectionbatch.ListResult{Schema: projectionbatch.ListSchema}, nil
}

func (service *countingBatchService) Show(
	context.Context,
	string,
) (projectionbatch.DetailResult, error) {
	service.record()
	return projectionbatch.DetailResult{Schema: projectionbatch.DetailSchema}, nil
}

func (service *countingBatchService) Undo(
	context.Context,
	projectionbatch.UndoRequest,
) (projectionbatch.UndoResult, error) {
	service.record()
	return projectionbatch.UndoResult{Schema: projectionbatch.UndoSchema}, nil
}

func differentUint32(value uint32) uint32 {
	if value == ^uint32(0) {
		return value - 1
	}
	return value + 1
}
