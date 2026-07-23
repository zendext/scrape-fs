package sourcefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const requiredResolveFlags = unix.RESOLVE_BENEATH |
	unix.RESOLVE_NO_MAGICLINKS |
	unix.RESOLVE_NO_SYMLINKS

// ErrOpenat2Unavailable indicates that secure source opening cannot be provided.
var ErrOpenat2Unavailable = errors.New("secure source opening requires openat2")

// ErrUnsafePath indicates that the source path cannot be opened safely.
var ErrUnsafePath = errors.New("unsafe source path")

// ErrNotRegular indicates that the source is not a regular file.
var ErrNotRegular = errors.New("source is not a regular file")

// Identity contains the fields used to detect a changed source file.
type Identity struct {
	DeviceID int64
	Inode    int64
	Size     int64
	MtimeNS  int64
}

// Metadata contains the normalized path and cached source attributes.
type Metadata struct {
	Path string
	Identity
	AtimeNS int64
	CtimeNS int64
}

// File owns a securely opened read-only source descriptor.
type File struct {
	*os.File
	Metadata
}

type openat2Func func(int, string, *unix.OpenHow) (int, error)

// Open securely opens an absolute local path and returns its cached metadata.
func Open(path string) (*File, error) {
	return openWithOpenat2(path, unix.Openat2)
}

// OpenVerified securely opens a source and rejects a changed identity with ESTALE.
func OpenVerified(path string, expected Identity) (*File, error) {
	opened, err := Open(path)
	if err != nil {
		return nil, err
	}
	if opened.Identity == expected {
		return opened, nil
	}

	staleErr := fmt.Errorf("source file identity changed for %q: %w", opened.Path, unix.ESTALE)
	return nil, errors.Join(staleErr, opened.Close())
}

func openWithOpenat2(path string, openat2 openat2Func) (*File, error) {
	normalized, relative, err := normalizePath(path)
	if err != nil {
		return nil, err
	}

	rootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}

	fd, openErr := openat2(rootFD, relative, &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOCTTY),
		Resolve: requiredResolveFlags,
	})
	rootCloseErr := unix.Close(rootFD)
	if openErr != nil {
		return nil, errors.Join(openError(normalized, openErr), rootCloseErr)
	}
	if rootCloseErr != nil {
		return nil, errors.Join(
			fmt.Errorf("close filesystem root descriptor: %w", rootCloseErr),
			unix.Close(fd),
		)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(
			fmt.Errorf("inspect source file %q: %w", normalized, err),
			unix.Close(fd),
		)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.Join(
			fmt.Errorf("%w: %q", ErrNotRegular, normalized),
			unix.Close(fd),
		)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		return nil, errors.Join(
			fmt.Errorf("configure source file %q for blocking reads: %w", normalized, err),
			unix.Close(fd),
		)
	}
	if stat.Dev > uint64(^uint64(0)>>1) || stat.Ino > uint64(^uint64(0)>>1) {
		return nil, errors.Join(
			fmt.Errorf("source file identity for %q exceeds supported integer range", normalized),
			unix.Close(fd),
		)
	}

	file := os.NewFile(uintptr(fd), normalized)
	if file == nil {
		return nil, errors.Join(
			fmt.Errorf("create source file handle for %q", normalized),
			unix.Close(fd),
		)
	}
	return &File{
		File: file,
		Metadata: Metadata{
			Path: normalized,
			Identity: Identity{
				DeviceID: int64(stat.Dev),
				Inode:    int64(stat.Ino),
				Size:     stat.Size,
				MtimeNS:  stat.Mtim.Nano(),
			},
			AtimeNS: stat.Atim.Nano(),
			CtimeNS: stat.Ctim.Nano(),
		},
	}, nil
}

func normalizePath(path string) (string, string, error) {
	if !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("%w: path %q must be absolute", ErrUnsafePath, path)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", "", fmt.Errorf("%w: path contains NUL", ErrUnsafePath)
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return "", "", fmt.Errorf(
				"%w: path %q must not contain '..'",
				ErrUnsafePath,
				path,
			)
		}
	}

	normalized := filepath.Clean(path)
	relative := strings.TrimPrefix(normalized, string(filepath.Separator))
	if relative == "" {
		relative = "."
	}
	return normalized, relative, nil
}

func openError(path string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("%w: open %q: %v", ErrUnsafePath, path, err)
	}
	if errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.EPERM) {
		return fmt.Errorf(
			"%w: Linux 5.6 or newer and permission to use openat2 are required: %v",
			ErrOpenat2Unavailable,
			err,
		)
	}
	return fmt.Errorf("open source file %q securely: %w", path, err)
}
