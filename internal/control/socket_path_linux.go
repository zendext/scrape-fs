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
	parent, err := openSocketParent(parentPath, security)
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

func openSocketParent(path string, security serverSecurity) (*os.File, error) {
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
	if len(components) > 0 {
		if err := validateSocketAncestor(currentFD, currentPath, security); err != nil {
			return nil, errors.Join(err, unix.Close(currentFD))
		}
	}
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
		if stat.Uid != 0 && stat.Uid != security.daemonUID {
			return nil, errors.Join(
				fmt.Errorf(
					"control socket directory %q owner UID is %d, want root or daemon effective UID %d",
					currentPath,
					stat.Uid,
					security.daemonUID,
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
		if index < len(components)-1 {
			if err := validateSocketAncestor(currentFD, currentPath, security); err != nil {
				return nil, errors.Join(err, unix.Close(currentFD))
			}
		}
	}

	var stat unix.Stat_t
	if err := unix.Fstat(currentFD, &stat); err != nil {
		return nil, errors.Join(
			fmt.Errorf("inspect control socket parent %q: %w", path, err),
			unix.Close(currentFD),
		)
	}
	if stat.Uid != security.daemonUID {
		return nil, errors.Join(
			fmt.Errorf(
				"control socket parent %q owner UID is %d, want daemon effective UID %d",
				path,
				stat.Uid,
				security.daemonUID,
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

func validateSocketAncestor(fd int, path string, security serverSecurity) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect control socket ancestor %q: %w", path, err)
	}
	entries, err := readAccessACL(descriptorPathForFD(uintptr(fd)))
	if err != nil {
		return fmt.Errorf("read control socket ancestor ACL for %q: %w", path, err)
	}
	mode := os.FileMode(stat.Mode).Perm()
	if security.administratorUID != 0 &&
		security.administratorUID != security.daemonUID &&
		!aclGuaranteesUserAccess(
			entries,
			mode,
			stat.Uid,
			security.administratorUID,
			aclExecute,
		) {
		return fmt.Errorf(
			"control socket ancestor %q does not allow configured administrator UID %d traversal",
			path,
			security.administratorUID,
		)
	}
	if uid, ok := aclRestrictiveNamedUser(
		entries,
		aclExecute,
		stat.Uid,
		aclUndefinedID,
	); ok {
		return fmt.Errorf(
			"control socket ancestor %q has named user UID %d without traverse permission, overriding control group access",
			path,
			uid,
		)
	}
	if !aclGuaranteesGroupAccess(
		entries,
		mode,
		stat.Uid,
		stat.Gid,
		security.controlGroupID,
		aclExecute,
	) {
		return fmt.Errorf(
			"control socket ancestor %q does not allow control group GID %d traversal",
			path,
			security.controlGroupID,
		)
	}
	return nil
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

	return prepared.verifyMetadata(mode)
}

func (prepared *preparedSocketPath) verifyMetadata(mode os.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(
		int(prepared.parent.Fd()),
		prepared.name,
		&stat,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
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

func (prepared *preparedSocketPath) verifyRuntimeMetadata(
	mode os.FileMode,
	security serverSecurity,
) error {
	if err := prepared.verifyConfiguredSocket(); err != nil {
		return err
	}
	if err := prepared.verifyMetadata(mode); err != nil {
		return err
	}

	var parentStat unix.Stat_t
	if err := unix.Fstat(int(prepared.parent.Fd()), &parentStat); err != nil {
		return fmt.Errorf("inspect control socket parent runtime metadata: %w", err)
	}
	if parentStat.Uid != prepared.daemonUID ||
		os.FileMode(parentStat.Mode).Perm()&0o022 != 0 {
		return fmt.Errorf("control socket parent runtime metadata is unsafe")
	}
	if err := verifyRoleAccess(
		prepared.descriptorPath(),
		os.FileMode(parentStat.Mode).Perm(),
		parentStat.Uid,
		parentStat.Gid,
		security,
		aclExecute,
	); err != nil {
		return fmt.Errorf("verify control socket parent runtime access: %w", err)
	}

	var socketStat unix.Stat_t
	if err := unix.Fstatat(
		int(prepared.parent.Fd()),
		prepared.name,
		&socketStat,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fmt.Errorf("inspect control socket runtime access: %w", err)
	}
	if err := verifyRoleAccess(
		prepared.socketDescriptorPath(),
		os.FileMode(socketStat.Mode).Perm(),
		socketStat.Uid,
		socketStat.Gid,
		security,
		aclRead|aclWrite,
	); err != nil {
		return fmt.Errorf("verify control socket runtime access: %w", err)
	}
	return nil
}

func (prepared *preparedSocketPath) grantRoleAccess(security serverSecurity) error {
	parentPath := prepared.descriptorPath()
	var parentStat unix.Stat_t
	if err := unix.Fstat(int(prepared.parent.Fd()), &parentStat); err != nil {
		return fmt.Errorf("inspect control socket parent for ACL: %w", err)
	}
	parentACL, err := readAccessACL(parentPath)
	if err != nil {
		return err
	}
	parentGrants := make([]aclEntry, 0, 2)
	parentRepairedUID := aclUndefinedID
	if security.administratorUID != 0 &&
		security.administratorUID != security.daemonUID &&
		!aclGuaranteesUserAccess(
			parentACL,
			os.FileMode(parentStat.Mode).Perm(),
			parentStat.Uid,
			security.administratorUID,
			aclExecute,
		) {
		parentGrants = append(parentGrants, aclEntry{
			Tag:         aclUser,
			Permissions: aclExecute,
			ID:          security.administratorUID,
		})
		parentRepairedUID = security.administratorUID
	}
	if uid, ok := aclRestrictiveNamedUser(
		parentACL,
		aclExecute,
		parentStat.Uid,
		parentRepairedUID,
	); ok {
		return fmt.Errorf(
			"control socket parent ACL has named user UID %d without traverse permission, overriding control group access",
			uid,
		)
	}
	if !aclGuaranteesGroupAccess(
		parentACL,
		os.FileMode(parentStat.Mode).Perm(),
		parentStat.Uid,
		parentStat.Gid,
		security.controlGroupID,
		aclExecute,
	) {
		parentGrants = append(parentGrants, aclEntry{
			Tag:         aclGroup,
			Permissions: aclExecute,
			ID:          security.controlGroupID,
		})
	}

	var socketStat unix.Stat_t
	if err := unix.Fstatat(
		int(prepared.parent.Fd()),
		prepared.name,
		&socketStat,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fmt.Errorf("inspect control socket for ACL: %w", err)
	}
	socketACL, err := readAccessACL(prepared.socketDescriptorPath())
	if err != nil {
		return err
	}
	socketGrants := make([]aclEntry, 0, 2)
	socketRepairedUID := aclUndefinedID
	if security.administratorUID != 0 &&
		security.administratorUID != security.daemonUID &&
		!aclGuaranteesUserAccess(
			socketACL,
			os.FileMode(socketStat.Mode).Perm(),
			socketStat.Uid,
			security.administratorUID,
			aclRead|aclWrite,
		) {
		socketGrants = append(socketGrants, aclEntry{
			Tag:         aclUser,
			Permissions: aclRead | aclWrite,
			ID:          security.administratorUID,
		})
		socketRepairedUID = security.administratorUID
	}
	if uid, ok := aclRestrictiveNamedUser(
		socketACL,
		aclRead|aclWrite,
		socketStat.Uid,
		socketRepairedUID,
	); ok {
		return fmt.Errorf(
			"control socket ACL has named user UID %d without read/write permission, overriding control group access",
			uid,
		)
	}
	if !aclGuaranteesGroupAccess(
		socketACL,
		os.FileMode(socketStat.Mode).Perm(),
		socketStat.Uid,
		socketStat.Gid,
		security.controlGroupID,
		aclRead|aclWrite,
	) {
		socketGrants = append(socketGrants, aclEntry{
			Tag:         aclGroup,
			Permissions: aclRead | aclWrite,
			ID:          security.controlGroupID,
		})
	}
	if len(socketGrants) > 0 {
		if err := updateAccessACL(
			prepared.socketDescriptorPath(),
			os.FileMode(socketStat.Mode).Perm(),
			socketGrants,
		); err != nil {
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
	if err := verifyRoleAccess(
		parentPath,
		os.FileMode(parentStat.Mode).Perm(),
		parentStat.Uid,
		parentStat.Gid,
		security,
		aclExecute,
	); err != nil {
		return fmt.Errorf("verify control socket parent role access: %w", err)
	}
	if err := verifyRoleAccess(
		prepared.socketDescriptorPath(),
		os.FileMode(socketStat.Mode).Perm(),
		socketStat.Uid,
		socketStat.Gid,
		security,
		aclRead|aclWrite,
	); err != nil {
		return fmt.Errorf("verify control socket role access: %w", err)
	}
	return nil
}

func verifyRoleAccess(
	path string,
	mode os.FileMode,
	ownerUID uint32,
	ownerGID uint32,
	security serverSecurity,
	required uint16,
) error {
	entries, err := readAccessACL(path)
	if err != nil {
		return err
	}
	if security.administratorUID != 0 &&
		security.administratorUID != security.daemonUID &&
		!aclGuaranteesUserAccess(
			entries,
			mode,
			ownerUID,
			security.administratorUID,
			required,
		) {
		return fmt.Errorf(
			"configured administrator UID %d lacks permissions %#o",
			security.administratorUID,
			required,
		)
	}
	if uid, ok := aclRestrictiveNamedUser(
		entries,
		required,
		ownerUID,
		aclUndefinedID,
	); ok {
		return fmt.Errorf(
			"named user UID %d lacks permissions %#o and overrides control group access",
			uid,
			required,
		)
	}
	if !aclGuaranteesGroupAccess(
		entries,
		mode,
		ownerUID,
		ownerGID,
		security.controlGroupID,
		required,
	) {
		return fmt.Errorf(
			"control group GID %d lacks permissions %#o",
			security.controlGroupID,
			required,
		)
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
	return descriptorPathForFD(prepared.parent.Fd())
}

func (prepared *preparedSocketPath) socketDescriptorPath() string {
	return prepared.descriptorPath() + "/" + prepared.name
}

func (prepared *preparedSocketPath) Close() error {
	return prepared.parent.Close()
}

func descriptorPathForFD(fd uintptr) string {
	return "/proc/self/fd/" + strconv.FormatUint(uint64(fd), 10)
}
