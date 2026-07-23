package control

import (
	"errors"
	"fmt"
	"net"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const ControlGroupName = "scrapefs-control"

var ErrPermissionDenied = errors.New("control permission denied")

const maxPeerGroupsBytes = 1 << 20

type peerIdentity struct {
	PID    int32
	UID    uint32
	GID    uint32
	Groups []uint32
}

type peerIdentityReader func(*net.UnixConn) (peerIdentity, error)

type serverSecurity struct {
	daemonUID        uint32
	administratorUID uint32
	controlGroupID   uint32
}

type commandRole uint8

const (
	commandRoleUnknown commandRole = iota
	commandRoleControl
	commandRoleAdministrator
)

type groupLookup func(string) (string, error)

func systemGroupLookup(name string) (string, error) {
	group, err := user.LookupGroup(name)
	if err != nil {
		return "", err
	}
	return group.Gid, nil
}

func resolveGroupID(name string, lookup groupLookup) (uint32, error) {
	value, err := lookup(name)
	if err != nil {
		return 0, fmt.Errorf("resolve control group %q: %w", name, err)
	}
	id, err := strconv.ParseUint(value, 10, 32)
	if err != nil || id == uint64(aclUndefinedID) {
		return 0, fmt.Errorf("control group %q has invalid GID %q", name, value)
	}
	return uint32(id), nil
}

func (security serverSecurity) authorize(command string, identity peerIdentity) error {
	role := requiredCommandRole(command)
	if role == commandRoleUnknown {
		return nil
	}
	if identity.UID == 0 || identity.UID == security.administratorUID {
		return nil
	}
	if role == commandRoleControl &&
		(identity.GID == security.controlGroupID ||
			containsGroup(identity.Groups, security.controlGroupID)) {
		return nil
	}
	return fmt.Errorf("%w for command %q", ErrPermissionDenied, command)
}

func requiredCommandRole(command string) commandRole {
	switch {
	case command == "status",
		command == "doctor",
		strings.HasPrefix(command, "projection."):
		return commandRoleControl
	case command == "gc",
		strings.HasPrefix(command, "library."),
		strings.HasPrefix(command, "daemon."):
		return commandRoleAdministrator
	default:
		return commandRoleUnknown
	}
}

func containsGroup(groups []uint32, target uint32) bool {
	for _, group := range groups {
		if group == target {
			return true
		}
	}
	return false
}

func readPeerIdentity(connection *net.UnixConn) (peerIdentity, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return peerIdentity{}, fmt.Errorf("access control connection descriptor: %w", err)
	}

	var identity peerIdentity
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(
			int(fd),
			unix.SOL_SOCKET,
			unix.SO_PEERCRED,
		)
		if err != nil {
			credentialErr = fmt.Errorf("read control peer credentials: %w", err)
			return
		}
		groups, err := getsockoptPeerGroups(int(fd))
		if err != nil {
			credentialErr = fmt.Errorf("read control peer groups: %w", err)
			return
		}
		identity = peerIdentity{
			PID:    credentials.Pid,
			UID:    credentials.Uid,
			GID:    credentials.Gid,
			Groups: groups,
		}
	}); err != nil {
		return peerIdentity{}, fmt.Errorf("inspect control peer credentials: %w", err)
	}
	if credentialErr != nil {
		return peerIdentity{}, credentialErr
	}
	return identity, nil
}

func getsockoptPeerGroups(fd int) ([]uint32, error) {
	groupSize := uint32(unsafe.Sizeof(uint32(0)))
	groups := make([]uint32, 8)
	for {
		length := uint32(len(groups)) * groupSize
		_, _, errno := unix.Syscall6(
			unix.SYS_GETSOCKOPT,
			uintptr(fd),
			uintptr(unix.SOL_SOCKET),
			uintptr(unix.SO_PEERGROUPS),
			uintptr(unsafe.Pointer(&groups[0])),
			uintptr(unsafe.Pointer(&length)),
			0,
		)
		runtime.KeepAlive(groups)
		if errno == 0 {
			if length%groupSize != 0 || int(length) > len(groups)*int(groupSize) {
				return nil, fmt.Errorf("kernel returned invalid peer group length %d", length)
			}
			return groups[:length/groupSize], nil
		}
		if errno != unix.ERANGE {
			return nil, errno
		}
		if length == 0 || length%groupSize != 0 || length > maxPeerGroupsBytes {
			return nil, fmt.Errorf("kernel requested invalid peer group length %d", length)
		}
		groups = make([]uint32, length/groupSize)
	}
}
