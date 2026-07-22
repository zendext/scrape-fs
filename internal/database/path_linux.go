package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	runtimeDirectoryMode  = 0o755
	databaseDirectoryMode = 0o700
	privateFileMode       = 0o600
)

func acquireLock(path string) (*os.File, error) {
	parent, name, err := openParentDirectory(path, runtimeDirectoryMode, "daemon lock directory")
	if err != nil {
		return nil, fmt.Errorf("prepare daemon lock path %q: %w", path, err)
	}
	defer parent.Close()

	lock, err := openRegularFile(parent, name, privateFileMode, "daemon lock")
	if err != nil {
		return nil, fmt.Errorf("prepare daemon lock path %q: %w", path, err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: lock %q is held", ErrAlreadyRunning, path)
		}
		return nil, fmt.Errorf("acquire daemon lock %q: %w", path, err)
	}
	return lock, nil
}

func releaseLock(lock *os.File) error {
	if lock == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	closeErr := lock.Close()
	return errors.Join(unlockErr, closeErr)
}

func prepareDatabaseFile(path string) error {
	parent, name, err := openParentDirectory(path, databaseDirectoryMode, "database directory")
	if err != nil {
		return fmt.Errorf("prepare database path %q: %w", path, err)
	}
	defer parent.Close()

	file, err := openRegularFile(parent, name, privateFileMode, "database")
	if err != nil {
		return fmt.Errorf("prepare database path %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return err
	}

	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		sidecar, err := openExistingRegularFile(
			parent,
			name+suffix,
			privateFileMode,
			"database sidecar "+suffix,
		)
		if err != nil {
			return fmt.Errorf("prepare database path %q: %w", path, err)
		}
		if sidecar != nil {
			if err := sidecar.Close(); err != nil {
				return fmt.Errorf("close database sidecar %q: %w", suffix, err)
			}
		}
	}
	return nil
}

func openParentDirectory(path string, mode os.FileMode, label string) (*os.File, string, error) {
	if !filepath.IsAbs(path) {
		return nil, "", fmt.Errorf("%s path must be absolute", label)
	}
	if cleaned := filepath.Clean(path); cleaned != path {
		return nil, "", fmt.Errorf("%s path must be normalized; use %q", label, cleaned)
	}

	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	if len(components) < 2 || components[len(components)-1] == "" {
		return nil, "", fmt.Errorf("%s path must have a non-root parent directory", label)
	}
	name := components[len(components)-1]
	components = components[:len(components)-1]

	currentFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open filesystem root: %w", err)
	}
	success := false
	defer func() {
		if !success {
			unix.Close(currentFD)
		}
	}()

	// Keep each parent directory open while resolving the next component so
	// no path component can be replaced by a symlink between checks.
	for index, component := range components {
		nextFD, openErr := unix.Openat(
			currentFD,
			component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		created := false
		if errors.Is(openErr, unix.ENOENT) {
			mkdirErr := unix.Mkdirat(currentFD, component, uint32(mode.Perm()))
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return nil, "", fmt.Errorf("create directory component %q: %w", component, mkdirErr)
			}
			created = mkdirErr == nil
			nextFD, openErr = unix.Openat(
				currentFD,
				component,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
				0,
			)
		}
		if openErr != nil {
			if pathComponentIsSymlink(currentFD, component) {
				return nil, "", fmt.Errorf("directory component %q is a symlink", component)
			}
			return nil, "", fmt.Errorf("open directory component %q: %w", component, openErr)
		}

		if err := unix.Close(currentFD); err != nil {
			unix.Close(nextFD)
			return nil, "", fmt.Errorf("close parent directory: %w", err)
		}
		currentFD = nextFD

		if created {
			if err := unix.Fchmod(currentFD, uint32(mode.Perm())); err != nil {
				return nil, "", fmt.Errorf("set directory component %q mode: %w", component, err)
			}
		}
		if index == len(components)-1 {
			if err := validateDirectory(currentFD, label, mode); err != nil {
				return nil, "", err
			}
		}
	}

	success = true
	return os.NewFile(uintptr(currentFD), filepath.Dir(path)), name, nil
}

func openRegularFile(parent *os.File, name string, mode os.FileMode, label string) (*os.File, error) {
	// O_EXCL distinguishes a file we created from an existing file whose
	// ownership and mode must be validated rather than repaired.
	flags := unix.O_RDWR | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(int(parent.Fd()), name, flags, uint32(mode.Perm()))
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(
			int(parent.Fd()),
			name,
			unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
	}
	if err != nil {
		if fileType, ok := pathComponentType(int(parent.Fd()), name); ok && fileType == unix.S_IFLNK {
			return nil, fmt.Errorf("%s is a symlink", label)
		}
		if fileType, ok := pathComponentType(int(parent.Fd()), name); ok && fileType != unix.S_IFREG {
			return nil, fmt.Errorf("%s must be a regular file", label)
		}
		return nil, fmt.Errorf("open %s: %w", label, err)
	}

	file := os.NewFile(uintptr(fd), name)
	if created {
		if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
			file.Close()
			return nil, fmt.Errorf("set %s mode: %w", label, err)
		}
	}
	if err := validateRegularFile(fd, label, mode); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func openExistingRegularFile(parent *os.File, name string, mode os.FileMode, label string) (*os.File, error) {
	fd, err := unix.Openat(
		int(parent.Fd()),
		name,
		unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		if fileType, ok := pathComponentType(int(parent.Fd()), name); ok && fileType == unix.S_IFLNK {
			return nil, fmt.Errorf("%s is a symlink", label)
		}
		if fileType, ok := pathComponentType(int(parent.Fd()), name); ok && fileType != unix.S_IFREG {
			return nil, fmt.Errorf("%s must be a regular file", label)
		}
		return nil, fmt.Errorf("open %s: %w", label, err)
	}

	file := os.NewFile(uintptr(fd), name)
	if err := validateRegularFile(fd, label, mode); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func validateRegularFile(fd int, label string, mode os.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s must be a regular file", label)
	}
	if err := validateOwner(label, &stat); err != nil {
		return err
	}
	if got := os.FileMode(stat.Mode).Perm(); got != mode.Perm() {
		return fmt.Errorf("%s mode is %#o, want %#o", label, got, mode.Perm())
	}
	return nil
}

func validateDirectory(fd int, label string, mode os.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if err := validateOwner(label, &stat); err != nil {
		return err
	}
	if got := os.FileMode(stat.Mode).Perm(); got != mode.Perm() {
		return fmt.Errorf("%s mode is %#o, want %#o", label, got, mode.Perm())
	}
	return nil
}

func validateOwner(label string, stat *unix.Stat_t) error {
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s owner UID is %d, want daemon effective UID %d", label, stat.Uid, os.Geteuid())
	}
	return nil
}

func pathComponentIsSymlink(parentFD int, name string) bool {
	fileType, ok := pathComponentType(parentFD, name)
	return ok && fileType == unix.S_IFLNK
}

func pathComponentType(parentFD int, name string) (uint32, bool) {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, false
	}
	return stat.Mode & unix.S_IFMT, true
}
