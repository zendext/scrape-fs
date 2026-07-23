package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	rootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root for control socket: %w", err)
	}
	relative := strings.TrimPrefix(path, "/")
	if relative == "" {
		relative = "."
	}
	fd, openErr := unix.Openat2(rootFD, relative, &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: socketResolveFlags,
	})
	rootCloseErr := unix.Close(rootFD)
	if openErr != nil {
		return nil, errors.Join(
			classifySocketParentOpenError(path, openErr),
			rootCloseErr,
		)
	}
	if rootCloseErr != nil {
		return nil, errors.Join(
			fmt.Errorf("close filesystem root for control socket: %w", rootCloseErr),
			unix.Close(fd),
		)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(
			fmt.Errorf("inspect control socket parent %q: %w", path, err),
			unix.Close(fd),
		)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, errors.Join(
			fmt.Errorf("control socket parent %q is not a directory", path),
			unix.Close(fd),
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
			unix.Close(fd),
		)
	}
	if stat.Mode&0o022 != 0 {
		return nil, errors.Join(
			fmt.Errorf(
				"control socket parent %q must not be writable by group or other",
				path,
			),
			unix.Close(fd),
		)
	}

	parent := os.NewFile(uintptr(fd), path)
	if parent == nil {
		return nil, errors.Join(
			fmt.Errorf("create control socket parent handle for %q", path),
			unix.Close(fd),
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

func (prepared *preparedSocketPath) Close() error {
	return prepared.parent.Close()
}
