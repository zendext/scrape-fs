package objectstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	directoryMode    = 0o700
	objectMode       = 0o600
	stagingDirectory = "staging"
	liveDirectory    = "live"
)

func prepareRoot(path string) (_ *os.File, err error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("object-store path must be absolute")
	}
	if cleaned := filepath.Clean(path); cleaned != path {
		return nil, fmt.Errorf("object-store path must be normalized; use %q", cleaned)
	}
	if path == string(filepath.Separator) {
		return nil, fmt.Errorf("object-store path must not be the filesystem root")
	}

	components := strings.Split(
		strings.TrimPrefix(path, string(filepath.Separator)),
		string(filepath.Separator),
	)
	currentFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, unix.Close(currentFD))
		}
	}()

	for index, component := range components {
		nextFD, created, openErr := openOrCreateDirectory(currentFD, component)
		if openErr != nil {
			return nil, fmt.Errorf("prepare object-store directory component %q: %w", component, openErr)
		}
		if closeErr := unix.Close(currentFD); closeErr != nil {
			unix.Close(nextFD)
			return nil, fmt.Errorf("close object-store parent directory: %w", closeErr)
		}
		currentFD = nextFD

		if created {
			if err := setCreatedOwnerAndMode(currentFD, directoryMode); err != nil {
				return nil, fmt.Errorf("secure object-store directory component %q: %w", component, err)
			}
		}
		if index == len(components)-1 {
			if err := validatePrivateDirectory(currentFD, "object-store root"); err != nil {
				return nil, err
			}
		}
	}

	root := os.NewFile(uintptr(currentFD), path)
	if root == nil {
		return nil, fmt.Errorf("create object-store root handle")
	}
	currentFD = -1
	return root, nil
}

func openPrivateSubdirectory(parent *os.File, name string) (*os.File, error) {
	fd, created, err := openOrCreateDirectory(int(parent.Fd()), name)
	if err != nil {
		return nil, fmt.Errorf("prepare object-store %s directory: %w", name, err)
	}
	directory := os.NewFile(uintptr(fd), name)
	if directory == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("create object-store %s directory handle", name)
	}
	if created {
		if err := setCreatedOwnerAndMode(fd, directoryMode); err != nil {
			directory.Close()
			return nil, fmt.Errorf("secure object-store %s directory: %w", name, err)
		}
	}
	if err := validatePrivateDirectory(fd, "object-store "+name+" directory"); err != nil {
		directory.Close()
		return nil, err
	}
	return directory, nil
}

func openOrCreateDirectory(parentFD int, name string) (int, bool, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(parentFD, name, flags, 0)
	created := false
	if errors.Is(err, unix.ENOENT) {
		if mkdirErr := unix.Mkdirat(parentFD, name, directoryMode); mkdirErr != nil &&
			!errors.Is(mkdirErr, unix.EEXIST) {
			return -1, false, fmt.Errorf("create directory: %w", mkdirErr)
		} else {
			created = mkdirErr == nil
		}
		fd, err = unix.Openat(parentFD, name, flags, 0)
	}
	if err != nil {
		if fileType, ok := entryType(parentFD, name); ok {
			switch fileType {
			case unix.S_IFLNK:
				return -1, false, fmt.Errorf("path is a symlink")
			case unix.S_IFDIR:
			default:
				return -1, false, fmt.Errorf("path must be a directory")
			}
		}
		return -1, false, fmt.Errorf("open directory: %w", err)
	}
	return fd, created, nil
}

func validateStoredObjects(directory *os.File, label string) error {
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list object-store %s directory: %w", label, err)
	}
	for _, name := range names {
		objectID, err := strconv.ParseInt(name, 10, 64)
		if err != nil || objectID <= 0 || strconv.FormatInt(objectID, 10) != name {
			return fmt.Errorf("object-store %s entry %q is not a canonical object ID", label, name)
		}
		file, err := openExistingObject(directory, objectID, unix.O_RDONLY)
		if err != nil {
			return fmt.Errorf("validate object-store %s object %d: %w", label, objectID, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close object-store %s object %d: %w", label, objectID, err)
		}
	}
	return nil
}

func createStagingFile(directory *os.File, objectID int64) (*os.File, error) {
	name := strconv.FormatInt(objectID, 10)
	fd, err := unix.Openat(
		int(directory.Fd()),
		name,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		objectMode,
	)
	if err != nil {
		return nil, fmt.Errorf("create staging object %d: %w", objectID, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("create staging object %d handle", objectID)
	}
	if err := setCreatedOwnerAndMode(fd, objectMode); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure staging object %d: %w", objectID, err)
	}
	if err := validatePrivateObject(fd); err != nil {
		file.Close()
		return nil, fmt.Errorf("validate staging object %d: %w", objectID, err)
	}
	return file, nil
}

func openExistingObject(directory *os.File, objectID int64, flags int) (*os.File, error) {
	name := strconv.FormatInt(objectID, 10)
	fd, err := unix.Openat(
		int(directory.Fd()),
		name,
		flags|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		if fileType, ok := entryType(int(directory.Fd()), name); ok {
			switch fileType {
			case unix.S_IFLNK:
				return nil, fmt.Errorf("object is a symlink")
			case unix.S_IFREG:
			default:
				return nil, fmt.Errorf("object must be a regular file")
			}
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("create object %d handle", objectID)
	}
	if err := validatePrivateObject(fd); err != nil {
		file.Close()
		return nil, err
	}
	if flags&unix.O_NONBLOCK == 0 {
		if err := unix.SetNonblock(fd, false); err != nil {
			file.Close()
			return nil, fmt.Errorf("configure object for blocking I/O: %w", err)
		}
	}
	return file, nil
}

func validatePrivateDirectory(fd int, label string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%s must be a directory", label)
	}
	return validatePrivateOwnerAndMode(label, &stat, directoryMode)
}

func validatePrivateObject(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect object: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("object must be a regular file")
	}
	return validatePrivateOwnerAndMode("object", &stat, objectMode)
}

func validatePrivateOwnerAndMode(label string, stat *unix.Stat_t, mode os.FileMode) error {
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		return fmt.Errorf(
			"%s owner is %d:%d, want daemon effective owner %d:%d",
			label,
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}
	if got := os.FileMode(stat.Mode).Perm(); got != mode.Perm() {
		return fmt.Errorf("%s mode is %#o, want %#o", label, got, mode.Perm())
	}
	return nil
}

func setCreatedOwnerAndMode(fd int, mode os.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect created path: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		if err := unix.Fchown(fd, os.Geteuid(), os.Getegid()); err != nil {
			return fmt.Errorf("set owner: %w", err)
		}
	}
	if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
		return fmt.Errorf("set mode: %w", err)
	}
	return nil
}

func entryType(parentFD int, name string) (uint32, bool) {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, false
	}
	return stat.Mode & unix.S_IFMT, true
}
