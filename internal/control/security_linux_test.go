package control

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
	"golang.org/x/sys/unix"
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
			command:  "library.create",
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
			name:     "control group lists libraries",
			command:  "library.list",
			identity: peerIdentity{UID: 1000, GID: 84},
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

	socketPath := filepath.Join(controlSocketTempDir(t), "peer.sock")
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
	socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	ready := make(chan struct{})
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
				ready: ready,
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

func TestServeRejectsControlUserLibraryCreationBeforeDispatch(t *testing.T) {
	libraryService := &countingLibraryService{}
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
						UID: differentUint32(42),
						GID: controlGroupID,
					}, nil
				},
				ready:          ready,
				libraryService: libraryService,
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

	_, err := NewClient(socketPath).CreateLibrary(
		context.Background(),
		"media",
		library.ProfileIdentifier{Name: "jellyfin", Version: 1},
	)
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("CreateLibrary error = %v, want RemoteError", err)
	}
	if remote.Code != "PermissionDenied" {
		t.Errorf("RemoteError code = %q, want PermissionDenied", remote.Code)
	}
	if libraryService.Calls() != 0 {
		t.Errorf("Library service calls = %d, want 0", libraryService.Calls())
	}
}

func TestServeRejectsControlUserLibraryLifecycleMutationsBeforeDispatch(t *testing.T) {
	libraryService := &countingLibraryService{}
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
						UID: differentUint32(42),
						GID: controlGroupID,
					}, nil
				},
				ready:          ready,
				libraryService: libraryService,
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
			_, err := client.RenameLibrary(context.Background(), "media", "movies")
			return err
		},
		func() error {
			_, err := client.SetLibraryProfile(
				context.Background(),
				"media",
				library.ProfileIdentifier{Name: "plex", Version: 1},
			)
			return err
		},
	}
	for _, call := range calls {
		err := call()
		var remote *RemoteError
		if !errors.As(err, &remote) {
			t.Fatalf("Library lifecycle error = %v, want RemoteError", err)
		}
		if remote.Code != "PermissionDenied" {
			t.Errorf("RemoteError code = %q, want PermissionDenied", remote.Code)
		}
	}
	if libraryService.Calls() != 0 {
		t.Errorf("Library service calls = %d, want 0", libraryService.Calls())
	}
}

func TestServeAllowsControlGroupRequest(t *testing.T) {
	service := &countingBatchService{}
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
				ready: ready,
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

func TestServeSetsSocketMetadataAndRoleAccessACLs(t *testing.T) {
	parent := controlSocketTempDir(t)
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	socketPath := filepath.Join(parent, "control.sock")
	administratorUID := differentUint32(uint32(os.Geteuid()))
	controlGroupID := uint32(os.Getegid())
	unrelatedUID := differentUint32(administratorUID)
	setACLXattr(t, parent, aclAccessXattr, []aclEntry{
		{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
		{Tag: aclUser, Permissions: aclExecute, ID: unrelatedUID},
		{Tag: aclGroupObject, Permissions: 0, ID: aclUndefinedID},
		{Tag: aclMask, Permissions: aclExecute, ID: aclUndefinedID},
		{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
	})
	setACLXattr(t, parent, aclDefaultXattr, []aclEntry{
		{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
		{Tag: aclUser, Permissions: aclRead | aclWrite, ID: unrelatedUID},
		{Tag: aclGroupObject, Permissions: 0, ID: aclUndefinedID},
		{Tag: aclMask, Permissions: aclRead | aclWrite, ID: aclUndefinedID},
		{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
	})
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
					administratorUID: administratorUID,
					controlGroupID:   controlGroupID,
				},
				readPeerIdentity: readPeerIdentity,
				ready:            ready,
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

	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat socket: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("socket stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != controlGroupID {
		t.Errorf(
			"socket ownership = %d:%d, want %d:%d",
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			controlGroupID,
		)
	}
	if info.Mode().Perm() != fs.FileMode(0o660) {
		t.Errorf("socket mode = %#o, want 0660", info.Mode().Perm())
	}

	socketACL, err := readAccessACL(socketPath)
	if err != nil {
		t.Fatalf("read socket ACL: %v", err)
	}
	requireACLEntry(t, socketACL, aclUser, administratorUID, aclRead|aclWrite)
	requireACLEntry(t, socketACL, aclUser, unrelatedUID, aclRead|aclWrite)
	requireACLEntry(t, socketACL, aclGroup, controlGroupID, aclRead|aclWrite)

	parentACL, err := readAccessACL(parent)
	if err != nil {
		t.Fatalf("read parent ACL: %v", err)
	}
	requireACLEntry(t, parentACL, aclUser, administratorUID, aclExecute)
	requireACLEntry(t, parentACL, aclUser, unrelatedUID, aclExecute)
	requireACLEntry(t, parentACL, aclGroup, controlGroupID, aclExecute)

	parentInfo, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if parentInfo.Mode().Perm()&0o001 != 0 {
		t.Errorf("parent mode = %#o, unexpectedly grants other traversal", parentInfo.Mode().Perm())
	}
}

func TestAuthorizedRolesConnectThroughExtendedACL(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root or a root-mapped user namespace")
	}

	const administratorUID uint32 = 1
	const controlGroupID uint32 = 2
	const controlUserUID uint32 = 3
	const unrelatedUID uint32 = 4
	parent := controlSocketTempDir(t)
	if err := os.Chown(parent, 0, int(controlGroupID)); err != nil {
		t.Fatalf("Chown parent: %v", err)
	}
	setACLXattr(t, parent, aclAccessXattr, []aclEntry{
		{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
		{Tag: aclUser, Permissions: aclExecute, ID: unrelatedUID},
		{Tag: aclGroupObject, Permissions: 0, ID: aclUndefinedID},
		{Tag: aclMask, Permissions: aclExecute, ID: aclUndefinedID},
		{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
	})
	setACLXattr(t, parent, aclDefaultXattr, []aclEntry{
		{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
		{Tag: aclUser, Permissions: aclRead | aclWrite, ID: unrelatedUID},
		{Tag: aclGroupObject, Permissions: 0, ID: aclUndefinedID},
		{Tag: aclMask, Permissions: aclRead | aclWrite, ID: aclUndefinedID},
		{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
	})
	socketPath := filepath.Join(parent, "control.sock")
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
					administratorUID: administratorUID,
					controlGroupID:   controlGroupID,
				},
				readPeerIdentity: readPeerIdentity,
				ready:            ready,
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

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	executableData, err := os.ReadFile(executable)
	if err != nil {
		t.Fatalf("ReadFile executable: %v", err)
	}
	clientDir := controlSocketTempDir(t)
	if err := os.Chmod(clientDir, 0o755); err != nil {
		t.Fatalf("Chmod client directory: %v", err)
	}
	clientExecutable := filepath.Join(clientDir, "control-acl-client")
	if err := os.WriteFile(clientExecutable, executableData, 0o755); err != nil {
		t.Fatalf("WriteFile client executable: %v", err)
	}

	runControlACLClient(
		t,
		clientExecutable,
		socketPath,
		administratorUID,
		administratorUID,
		[]uint32{administratorUID},
	)
	runControlACLClient(
		t,
		clientExecutable,
		socketPath,
		controlUserUID,
		controlGroupID,
		[]uint32{controlGroupID},
	)

	socketACL, err := readAccessACL(socketPath)
	if err != nil {
		t.Fatalf("read socket ACL: %v", err)
	}
	socketACL = append(socketACL, aclEntry{
		Tag:         aclUser,
		Permissions: 0,
		ID:          controlUserUID,
	})
	sort.Slice(socketACL, func(i, j int) bool {
		if socketACL[i].Tag == socketACL[j].Tag {
			return socketACL[i].ID < socketACL[j].ID
		}
		return socketACL[i].Tag < socketACL[j].Tag
	})
	setACLXattr(t, socketPath, aclAccessXattr, socketACL)
	runDeniedControlACLClient(
		t,
		clientExecutable,
		socketPath,
		controlUserUID,
		controlGroupID,
		[]uint32{controlGroupID},
	)
}

func TestAuthorizedRoleACLClient(t *testing.T) {
	socketPath := os.Getenv("SCRAPEFS_CONTROL_ACL_TEST_SOCKET")
	if socketPath == "" {
		t.Skip("helper process")
	}
	if _, err := NewClient(socketPath).List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestDeniedControlACLClient(t *testing.T) {
	socketPath := os.Getenv("SCRAPEFS_CONTROL_ACL_TEST_SOCKET")
	if socketPath == "" {
		t.Skip("helper process")
	}
	connection, err := net.DialUnix(
		"unix",
		nil,
		&net.UnixAddr{Name: socketPath, Net: "unix"},
	)
	if err == nil {
		connection.Close()
		t.Fatal("DialUnix succeeded through restrictive named-user ACL")
	}
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("DialUnix error = %v, want EACCES", err)
	}
}

func TestServeRejectsRestrictiveNamedUserACL(t *testing.T) {
	const restrictiveUID uint32 = 2000
	controlGroupID := uint32(os.Getegid())
	tests := []struct {
		name          string
		parentACL     []aclEntry
		parentDefault []aclEntry
		want          string
	}{
		{
			name: "parent",
			parentACL: []aclEntry{
				{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
				{Tag: aclUser, Permissions: 0, ID: restrictiveUID},
				{Tag: aclGroupObject, Permissions: aclExecute, ID: aclUndefinedID},
				{Tag: aclGroup, Permissions: aclExecute, ID: controlGroupID},
				{Tag: aclMask, Permissions: aclExecute, ID: aclUndefinedID},
				{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
			},
			want: "parent",
		},
		{
			name: "inherited socket",
			parentACL: []aclEntry{
				{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
				{Tag: aclGroupObject, Permissions: aclExecute, ID: aclUndefinedID},
				{Tag: aclGroup, Permissions: aclExecute, ID: controlGroupID},
				{Tag: aclMask, Permissions: aclExecute, ID: aclUndefinedID},
				{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
			},
			parentDefault: []aclEntry{
				{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
				{Tag: aclUser, Permissions: 0, ID: restrictiveUID},
				{Tag: aclGroupObject, Permissions: aclRead | aclWrite, ID: aclUndefinedID},
				{Tag: aclGroup, Permissions: aclRead | aclWrite, ID: controlGroupID},
				{Tag: aclMask, Permissions: aclRead | aclWrite, ID: aclUndefinedID},
				{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
			},
			want: "socket",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			parent := controlSocketTempDir(t)
			setACLXattr(t, parent, aclAccessXattr, test.parentACL)
			if len(test.parentDefault) > 0 {
				setACLXattr(t, parent, aclDefaultXattr, test.parentDefault)
			}
			socketPath := filepath.Join(parent, "control.sock")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
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
							administratorUID: uint32(os.Geteuid()),
							controlGroupID:   controlGroupID,
						},
						readPeerIdentity: readPeerIdentity,
						ready:            ready,
					},
				)
			}()

			select {
			case <-ready:
				t.Fatal("server became ready with a restrictive named-user ACL")
			case err := <-serverDone:
				if err == nil ||
					!strings.Contains(err.Error(), test.want) ||
					!strings.Contains(err.Error(), "named user") {
					t.Fatalf("serveWithOptions error = %v, want %s named-user ACL error", err, test.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server neither failed nor became ready")
			}
		})
	}
}

func TestServeSkipsParentACLWhenModeAlreadyAllowsTraversal(t *testing.T) {
	parent := controlSocketTempDir(t)
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	socketPath := filepath.Join(parent, "control.sock")
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
					administratorUID: differentUint32(uint32(os.Geteuid())),
					controlGroupID:   uint32(os.Getegid()),
				},
				readPeerIdentity: readPeerIdentity,
				ready:            ready,
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

	parentACL, err := readAccessACL(parent)
	if err != nil {
		t.Fatalf("read parent ACL: %v", err)
	}
	if len(parentACL) != 0 {
		t.Errorf("parent ACL = %#v, want no ACL when mode already allows traversal", parentACL)
	}
}

func TestPrepareSocketPathRejectsUnsafeOrUnexpectedExistingPaths(t *testing.T) {
	t.Parallel()

	security := serverSecurity{
		daemonUID:      uint32(os.Geteuid()),
		controlGroupID: uint32(os.Getegid()),
	}
	t.Run("symlink parent", func(t *testing.T) {
		root := controlSocketTempDir(t)
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatalf("Chmod root: %v", err)
		}
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
		root := controlSocketTempDir(t)
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
		parent := controlSocketTempDir(t)
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

	t.Run("non-sticky writable ancestor", func(t *testing.T) {
		root := controlSocketTempDir(t)
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatalf("Chmod root: %v", err)
		}
		ancestor := filepath.Join(root, "unsafe")
		if err := os.Mkdir(ancestor, 0o770); err != nil {
			t.Fatalf("Mkdir ancestor: %v", err)
		}
		if err := os.Chmod(ancestor, 0o770); err != nil {
			t.Fatalf("Chmod ancestor: %v", err)
		}
		parent := filepath.Join(ancestor, "run")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("Mkdir parent: %v", err)
		}

		prepared, err := prepareSocketPath(filepath.Join(parent, "control.sock"), security)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "writable ancestor") {
			t.Fatalf("prepareSocketPath error = %v, want writable-ancestor error", err)
		}
	})

	t.Run("non-traversable ancestor", func(t *testing.T) {
		root := controlSocketTempDir(t)
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatalf("Chmod root: %v", err)
		}
		ancestor := filepath.Join(root, "private")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatalf("Mkdir ancestor: %v", err)
		}
		parent := filepath.Join(ancestor, "run")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("Mkdir parent: %v", err)
		}

		prepared, err := prepareSocketPath(filepath.Join(parent, "control.sock"), security)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "does not allow control group GID") {
			t.Fatalf("prepareSocketPath error = %v, want traversal error", err)
		}
	})

	t.Run("ancestor with role ACLs", func(t *testing.T) {
		root := controlSocketTempDir(t)
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatalf("Chmod root: %v", err)
		}
		ancestor := filepath.Join(root, "private")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatalf("Mkdir ancestor: %v", err)
		}
		allowed := security
		allowed.administratorUID = differentUint32(uint32(os.Geteuid()))
		if err := updateAccessACL(ancestor, 0o700, []aclEntry{
			{Tag: aclUser, Permissions: aclExecute, ID: allowed.administratorUID},
			{Tag: aclGroup, Permissions: aclExecute, ID: allowed.controlGroupID},
		}); err != nil {
			t.Fatalf("updateAccessACL ancestor: %v", err)
		}
		parent := filepath.Join(ancestor, "run")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("Mkdir parent: %v", err)
		}

		prepared, err := prepareSocketPath(filepath.Join(parent, "control.sock"), allowed)
		if err != nil {
			t.Fatalf("prepareSocketPath: %v", err)
		}
		prepared.Close()
	})

	t.Run("ancestor with restrictive named user", func(t *testing.T) {
		root := controlSocketTempDir(t)
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatalf("Chmod root: %v", err)
		}
		ancestor := filepath.Join(root, "private")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatalf("Mkdir ancestor: %v", err)
		}
		if err := updateAccessACL(ancestor, 0o700, []aclEntry{
			{Tag: aclUser, Permissions: 0, ID: 2000},
			{Tag: aclGroup, Permissions: aclExecute, ID: security.controlGroupID},
		}); err != nil {
			t.Fatalf("updateAccessACL ancestor: %v", err)
		}
		parent := filepath.Join(ancestor, "run")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("Mkdir parent: %v", err)
		}

		prepared, err := prepareSocketPath(filepath.Join(parent, "control.sock"), security)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "named user") {
			t.Fatalf("prepareSocketPath error = %v, want named-user traversal error", err)
		}
	})

	t.Run("unexpected stale socket group", func(t *testing.T) {
		socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
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
		socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
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

func TestPreparedSocketPathAnchorsBindVerificationAndCleanup(t *testing.T) {
	root := controlSocketTempDir(t)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("Chmod root: %v", err)
	}
	parent := filepath.Join(root, "run")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir parent: %v", err)
	}
	socketPath := filepath.Join(parent, "control.sock")
	prepared, err := prepareSocketPath(socketPath, serverSecurity{
		daemonUID:      uint32(os.Geteuid()),
		controlGroupID: uint32(os.Getegid()),
	})
	if err != nil {
		t.Fatalf("prepareSocketPath: %v", err)
	}
	defer prepared.Close()

	validatedParent := parent + ".validated"
	if err := os.Rename(parent, validatedParent); err != nil {
		t.Fatalf("Rename validated parent: %v", err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir replacement parent: %v", err)
	}

	listener, err := prepared.listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	anchoredPath := filepath.Join(validatedParent, "control.sock")
	if _, err := os.Lstat(anchoredPath); err != nil {
		listener.Close()
		t.Fatalf("anchored socket was not created in retained parent: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		listener.Close()
		t.Fatalf("configured replacement path stat error = %v, want not exist", err)
	}

	replacement, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: socketPath, Net: "unix"},
	)
	if err != nil {
		listener.Close()
		t.Fatalf("ListenUnix replacement: %v", err)
	}
	defer replacement.Close()
	if err := prepared.verifyConfiguredSocket(); err == nil {
		listener.Close()
		t.Fatal("verifyConfiguredSocket succeeded after parent replacement")
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("Close anchored listener: %v", err)
	}
	if _, err := os.Lstat(anchoredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("anchored socket stat error = %v, want not exist", err)
	}
	if info, err := os.Lstat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("replacement socket was removed or changed: info=%v err=%v", info, err)
	}
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
		{
			name: "ACL undefined gid",
			lookup: func(string) (string, error) {
				return strconv.FormatUint(uint64(^uint32(0)), 10), nil
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

type countingLibraryService struct {
	mu    sync.Mutex
	calls int
}

func (service *countingLibraryService) Calls() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.calls
}

func (service *countingLibraryService) Create(
	context.Context,
	library.CreateRequest,
) (namespace.Library, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.calls++
	return namespace.Library{}, nil
}

func (service *countingLibraryService) List(
	context.Context,
) ([]namespace.Library, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.calls++
	return []namespace.Library{}, nil
}

func (service *countingLibraryService) Rename(
	context.Context,
	library.RenameRequest,
) (namespace.Library, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.calls++
	return namespace.Library{}, nil
}

func (service *countingLibraryService) SetProfile(
	context.Context,
	library.SetProfileRequest,
) (namespace.Library, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.calls++
	return namespace.Library{}, nil
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

func requireACLEntry(
	t *testing.T,
	entries []aclEntry,
	tag uint16,
	id uint32,
	permissions uint16,
) {
	t.Helper()

	for _, entry := range entries {
		if entry.Tag == tag && entry.ID == id {
			if entry.Permissions&permissions != permissions {
				t.Errorf(
					"ACL entry tag=%d id=%d permissions=%#o, want at least %#o",
					tag,
					id,
					entry.Permissions,
					permissions,
				)
			}
			return
		}
	}
	t.Errorf("ACL entry tag=%d id=%d not found in %#v", tag, id, entries)
}

func setACLXattr(t *testing.T, path, name string, entries []aclEntry) {
	t.Helper()

	if err := unix.Setxattr(path, name, encodeAccessACL(entries), 0); err != nil {
		t.Fatalf("Setxattr %s on %q: %v", name, path, err)
	}
}

func runControlACLClient(
	t *testing.T,
	executable string,
	socketPath string,
	uid uint32,
	gid uint32,
	groups []uint32,
) {
	t.Helper()

	command := exec.Command(
		executable,
		"-test.run=^TestAuthorizedRoleACLClient$",
	)
	command.Env = append(os.Environ(), "SCRAPEFS_CONTROL_ACL_TEST_SOCKET="+socketPath)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uid,
			Gid:    gid,
			Groups: groups,
		},
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ACL client %d:%d: %v\n%s", uid, gid, err, output)
	}
}

func runDeniedControlACLClient(
	t *testing.T,
	executable string,
	socketPath string,
	uid uint32,
	gid uint32,
	groups []uint32,
) {
	t.Helper()

	command := exec.Command(
		executable,
		"-test.run=^TestDeniedControlACLClient$",
	)
	command.Env = append(os.Environ(), "SCRAPEFS_CONTROL_ACL_TEST_SOCKET="+socketPath)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uid,
			Gid:    gid,
			Groups: groups,
		},
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("denied ACL client %d:%d: %v\n%s", uid, gid, err, output)
	}
}
