package control

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const socketResolveFlags = unix.RESOLVE_BENEATH |
	unix.RESOLVE_NO_MAGICLINKS |
	unix.RESOLVE_NO_SYMLINKS

type preparedSocketPath struct {
	parent     *os.File
	path       string
	name       string
	daemonUID  uint32
	controlGID uint32
}

func prepareSocketPath(
	socketPath string,
	security serverSecurity,
) (*preparedSocketPath, error) {
	if !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf("control socket path %q must be absolute", socketPath)
	}
	if strings.IndexByte(socketPath, 0) >= 0 {
		return nil, fmt.Errorf("control socket path contains NUL")
	}
	if cleaned := filepath.Clean(socketPath); cleaned != socketPath {
		return nil, fmt.Errorf(
			"control socket path %q must be lexically normalized; use %q",
			socketPath,
			cleaned,
		)
	}

	parentPath := filepath.Dir(socketPath)
	name := filepath.Base(socketPath)
	parent, err := openSocketParent(parentPath, security.daemonUID)
	if err != nil {
		return nil, err
	}
	prepared := &preparedSocketPath{
		parent:     parent,
		path:       socketPath,
		name:       name,
		daemonUID:  security.daemonUID,
		controlGID: security.controlGroupID,
	}
	if err := prepared.removeStale(); err != nil {
		parent.Close()
		return nil, err
	}
	return prepared, nil
}

func openSocketParent(path string, daemonUID uint32) (*os.File, error) {
	currentFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root for control socket: %w", err)
	}
	components := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(components) == 1 && components[0] == "" {
		components = nil
	}
	if len(components) == 0 {
		rootFD, openErr := unix.Openat2(currentFD, ".", &unix.OpenHow{
			Flags:   uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC),
			Resolve: socketResolveFlags,
		})
		if openErr != nil {
			return nil, errors.Join(
				classifySocketParentOpenError(path, openErr),
				unix.Close(currentFD),
			)
		}
		if err := unix.Close(currentFD); err != nil {
			return nil, errors.Join(
				fmt.Errorf("close filesystem root descriptor: %w", err),
				unix.Close(rootFD),
			)
		}
		currentFD = rootFD
	}
	currentPath := "/"
	for index, component := range components {
		flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC
		if index == len(components)-1 {
			flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
		}
		nextFD, openErr := unix.Openat2(currentFD, component, &unix.OpenHow{
			Flags:   uint64(flags),
			Resolve: socketResolveFlags,
		})
		if openErr != nil {
			return nil, errors.Join(
				classifySocketParentOpenError(path, openErr),
				unix.Close(currentFD),
			)
		}
		if err := unix.Close(currentFD); err != nil {
			return nil, errors.Join(
				fmt.Errorf("close control socket ancestor descriptor: %w", err),
				unix.Close(nextFD),
			)
		}
		currentFD = nextFD
		currentPath = filepath.Join(currentPath, component)

		var stat unix.Stat_t
		if err := unix.Fstat(currentFD, &stat); err != nil {
			return nil, errors.Join(
				fmt.Errorf("inspect control socket directory %q: %w", currentPath, err),
				unix.Close(currentFD),
			)
		}
		if stat.Uid != 0 && stat.Uid != daemonUID {
			return nil, errors.Join(
				fmt.Errorf(
					"control socket directory %q owner UID is %d, want root or daemon effective UID %d",
					currentPath,
					stat.Uid,
					daemonUID,
				),
				unix.Close(currentFD),
			)
		}
		if index < len(components)-1 &&
			stat.Mode&0o022 != 0 &&
			stat.Mode&unix.S_ISVTX == 0 {
			return nil, errors.Join(
				fmt.Errorf(
					"control socket path has untrusted writable ancestor %q",
					currentPath,
				),
				unix.Close(currentFD),
			)
		}
	}

	var stat unix.Stat_t
	if err := unix.Fstat(currentFD, &stat); err != nil {
		return nil, errors.Join(
			fmt.Errorf("inspect control socket parent %q: %w", path, err),
			unix.Close(currentFD),
		)
	}
	if stat.Uid != daemonUID {
		return nil, errors.Join(
			fmt.Errorf(
				"control socket parent %q owner UID is %d, want daemon effective UID %d",
				path,
				stat.Uid,
				daemonUID,
			),
			unix.Close(currentFD),
		)
	}
	if stat.Mode&0o022 != 0 {
		return nil, errors.Join(
			fmt.Errorf(
				"control socket parent %q must not be writable by group or other",
				path,
			),
			unix.Close(currentFD),
		)
	}
	if stat.Mode&0o100 == 0 {
		return nil, errors.Join(
			fmt.Errorf("control socket parent %q must be traversable by its owner", path),
			unix.Close(currentFD),
		)
	}

	parent := os.NewFile(uintptr(currentFD), path)
	if parent == nil {
		return nil, errors.Join(
			fmt.Errorf("create control socket parent handle for %q", path),
			unix.Close(currentFD),
		)
	}
	return parent, nil
}

func classifySocketParentOpenError(path string, err error) error {
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EXDEV):
		return fmt.Errorf("open control socket parent %q safely: %w", path, err)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.EPERM):
		return fmt.Errorf(
			"secure control socket paths require Linux openat2 support: %w",
			err,
		)
	default:
		return fmt.Errorf("open control socket parent %q: %w", path, err)
	}
}

func (prepared *preparedSocketPath) removeStale() error {
	var stat unix.Stat_t
	err := unix.Fstatat(
		int(prepared.parent.Fd()),
		prepared.name,
		&stat,
		unix.AT_SYMLINK_NOFOLLOW,
	)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect ScrapeFS control socket %q: %w", prepared.path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf(
			"ScrapeFS control socket path %q exists and is not a socket",
			prepared.path,
		)
	}
	if stat.Uid != prepared.daemonUID || stat.Gid != prepared.controlGID {
		return fmt.Errorf(
			"ScrapeFS control socket %q has unexpected ownership %d:%d, want %d:%d",
			prepared.path,
			stat.Uid,
			stat.Gid,
			prepared.daemonUID,
			prepared.controlGID,
		)
	}
	if err := unix.Unlinkat(int(prepared.parent.Fd()), prepared.name, 0); err != nil {
		return fmt.Errorf("remove stale ScrapeFS control socket %q: %w", prepared.path, err)
	}
	return nil
}

func (prepared *preparedSocketPath) setMetadata(mode os.FileMode) error {
	parentFD := int(prepared.parent.Fd())
	if err := unix.Fchownat(
		parentFD,
		prepared.name,
		int(prepared.daemonUID),
		int(prepared.controlGID),
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fmt.Errorf("set ScrapeFS control socket ownership: %w", err)
	}
	if err := unix.Fchmodat(parentFD, prepared.name, uint32(mode.Perm()), 0); err != nil {
		return fmt.Errorf("set ScrapeFS control socket mode: %w", err)
	}

	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, prepared.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("verify ScrapeFS control socket metadata: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK ||
		stat.Uid != prepared.daemonUID ||
		stat.Gid != prepared.controlGID ||
		os.FileMode(stat.Mode).Perm() != mode.Perm() {
		return fmt.Errorf("ScrapeFS control socket metadata verification failed")
	}
	return nil
}

func (prepared *preparedSocketPath) grantRoleAccess(
	mode os.FileMode,
	security serverSecurity,
) error {
	parentPath := prepared.descriptorPath()
	var parentStat unix.Stat_t
	if err := unix.Fstat(int(prepared.parent.Fd()), &parentStat); err != nil {
		return fmt.Errorf("inspect control socket parent for ACL: %w", err)
	}
	parentGrants := make([]aclEntry, 0, 2)
	if security.administratorUID != 0 &&
		security.administratorUID != security.daemonUID &&
		uint16(parentStat.Mode)&aclExecute == 0 {
		parentGrants = append(parentGrants, aclEntry{
			Tag:         aclUser,
			Permissions: aclExecute,
			ID:          security.administratorUID,
		})
	}
	groupPermissions := uint16(parentStat.Mode>>3) & 0o7
	otherPermissions := uint16(parentStat.Mode) & 0o7
	if otherPermissions&aclExecute == 0 &&
		(parentStat.Gid != security.controlGroupID ||
			groupPermissions&aclExecute == 0) {
		parentGrants = append(parentGrants, aclEntry{
			Tag:         aclGroup,
			Permissions: aclExecute,
			ID:          security.controlGroupID,
		})
	}

	socketGrants := make([]aclEntry, 0, 1)
	if security.administratorUID != 0 &&
		security.administratorUID != security.daemonUID &&
		mode.Perm()&0o006 != 0o006 {
		socketGrants = append(socketGrants, aclEntry{
			Tag:         aclUser,
			Permissions: aclRead | aclWrite,
			ID:          security.administratorUID,
		})
	}
	if len(socketGrants) > 0 {
		if err := updateAccessACL(prepared.socketDescriptorPath(), mode, socketGrants); err != nil {
			return err
		}
	}
	if len(parentGrants) > 0 {
		if err := updateAccessACL(
			parentPath,
			os.FileMode(parentStat.Mode).Perm(),
			parentGrants,
		); err != nil {
			return err
		}
	}
	return nil
}

func (prepared *preparedSocketPath) listen() (*net.UnixListener, error) {
	address := &net.UnixAddr{Name: prepared.socketDescriptorPath(), Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, fmt.Errorf(
			"listen on ScrapeFS control socket %q through /proc/self/fd: %w",
			prepared.path,
			err,
		)
	}
	listener.SetUnlinkOnClose(true)
	return listener, nil
}

func (prepared *preparedSocketPath) verifyConfiguredSocket() error {
	var anchored unix.Stat_t
	if err := unix.Fstatat(
		int(prepared.parent.Fd()),
		prepared.name,
		&anchored,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fmt.Errorf("inspect anchored ScrapeFS control socket %q: %w", prepared.path, err)
	}
	var configured unix.Stat_t
	if err := unix.Lstat(prepared.path, &configured); err != nil {
		return fmt.Errorf("inspect configured ScrapeFS control socket %q: %w", prepared.path, err)
	}
	if configured.Mode&unix.S_IFMT != unix.S_IFSOCK ||
		configured.Dev != anchored.Dev ||
		configured.Ino != anchored.Ino {
		return fmt.Errorf(
			"configured ScrapeFS control socket path %q no longer resolves to the bound socket",
			prepared.path,
		)
	}
	return nil
}

func (prepared *preparedSocketPath) descriptorPath() string {
	return "/proc/self/fd/" + strconv.FormatUint(uint64(prepared.parent.Fd()), 10)
}

func (prepared *preparedSocketPath) socketDescriptorPath() string {
	return prepared.descriptorPath() + "/" + prepared.name
}

func (prepared *preparedSocketPath) Close() error {
	return prepared.parent.Close()
}
