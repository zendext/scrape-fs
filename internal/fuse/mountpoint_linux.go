package fuse

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	filesystemName          = "scrapefs"
	mountpointMode          = os.FileMode(0o710)
	mountpointAncestorMode  = os.FileMode(0o755)
	mountpointResolveFlags  = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS
	mountpointOtherModeMask = 0o007
)

type preparedMountpoint struct {
	directory *os.File
	path      string
	device    uint64
	inode     uint64
}

func (prepared *preparedMountpoint) Close() error {
	if prepared == nil || prepared.directory == nil {
		return nil
	}
	return prepared.directory.Close()
}

func (prepared *preparedMountpoint) withTarget(
	action func(string) error,
) error {
	return action(filepath.Join(
		"/proc/self/fd",
		strconv.FormatUint(uint64(prepared.directory.Fd()), 10),
	))
}

func (prepared *preparedMountpoint) verifyPath() error {
	rootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open filesystem root to verify FUSE mountpoint: %w", err)
	}
	defer unix.Close(rootFD)

	currentFD, err := unix.Openat2(
		rootFD,
		strings.TrimPrefix(prepared.path, "/"),
		&unix.OpenHow{
			Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
			Resolve: mountpointResolveFlags,
		},
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("FUSE mountpoint path %q contains a replaced symlink: %w", prepared.path, err)
		}
		return fmt.Errorf("reopen FUSE mountpoint path %q safely: %w", prepared.path, err)
	}
	defer unix.Close(currentFD)

	var current unix.Stat_t
	if err := unix.Fstat(currentFD, &current); err != nil {
		return fmt.Errorf("inspect reopened FUSE mountpoint path %q: %w", prepared.path, err)
	}
	if uint64(current.Dev) != prepared.device || current.Ino != prepared.inode {
		return fmt.Errorf("mountpoint path %q was replaced during preparation", prepared.path)
	}
	return nil
}

func prepareMountpointDirectory(path string) (_ *preparedMountpoint, err error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("FUSE mountpoint %q must be absolute", path)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("FUSE mountpoint contains NUL")
	}
	if cleaned := filepath.Clean(path); cleaned != path {
		return nil, fmt.Errorf(
			"FUSE mountpoint %q must be lexically normalized; use %q",
			path,
			cleaned,
		)
	}
	if path == "/" {
		return nil, errors.New("FUSE mountpoint must not be the filesystem root")
	}

	components := strings.Split(strings.TrimPrefix(path, "/"), "/")
	currentFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root for FUSE mountpoint: %w", err)
	}
	defer func() {
		if err != nil && currentFD >= 0 {
			err = errors.Join(err, unix.Close(currentFD))
		}
	}()

	currentPath := "/"
	for index, component := range components {
		final := index == len(components)-1
		flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC
		mode := mountpointAncestorMode
		if final {
			flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
			mode = mountpointMode
		}

		nextFD, openErr := unix.Openat2(currentFD, component, &unix.OpenHow{
			Flags:   uint64(flags),
			Resolve: mountpointResolveFlags,
		})
		created := false
		if errors.Is(openErr, unix.ENOENT) {
			mkdirErr := unix.Mkdirat(currentFD, component, uint32(mode.Perm()))
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return nil, fmt.Errorf(
					"create FUSE mountpoint directory %q: %w",
					filepath.Join(currentPath, component),
					mkdirErr,
				)
			}
			created = mkdirErr == nil
			if created {
				flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
			}
			nextFD, openErr = unix.Openat2(currentFD, component, &unix.OpenHow{
				Flags:   uint64(flags),
				Resolve: mountpointResolveFlags,
			})
		}
		if openErr != nil {
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.EXDEV) {
				return nil, fmt.Errorf(
					"FUSE mountpoint component %q is a symlink or escapes its parent: %w",
					filepath.Join(currentPath, component),
					openErr,
				)
			}
			if errors.Is(openErr, unix.ENOSYS) || errors.Is(openErr, unix.EINVAL) {
				return nil, fmt.Errorf(
					"secure FUSE mountpoints require Linux openat2 support: %w",
					openErr,
				)
			}
			return nil, fmt.Errorf(
				"open FUSE mountpoint component %q: %w",
				filepath.Join(currentPath, component),
				openErr,
			)
		}

		if created {
			if err := unix.Fchmod(nextFD, uint32(mode.Perm())); err != nil {
				unix.Close(nextFD)
				return nil, fmt.Errorf(
					"set FUSE mountpoint directory %q mode: %w",
					filepath.Join(currentPath, component),
					err,
				)
			}
		}
		if err := unix.Close(currentFD); err != nil {
			unix.Close(nextFD)
			return nil, fmt.Errorf("close FUSE mountpoint ancestor: %w", err)
		}
		currentFD = nextFD
		currentPath = filepath.Join(currentPath, component)

		if err := validateMountpointDirectory(currentFD, currentPath, final); err != nil {
			return nil, err
		}
	}

	directory := os.NewFile(uintptr(currentFD), path)
	if directory == nil {
		return nil, fmt.Errorf("create FUSE mountpoint handle for %q", path)
	}
	currentFD = -1

	entries, readErr := directory.Readdirnames(1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		directory.Close()
		return nil, fmt.Errorf("read FUSE mountpoint %q: %w", path, readErr)
	}
	if len(entries) != 0 {
		directory.Close()
		return nil, fmt.Errorf("FUSE mountpoint %q must be empty", path)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		directory.Close()
		return nil, fmt.Errorf("inspect FUSE mountpoint %q: %w", path, err)
	}
	return &preparedMountpoint{
		directory: directory,
		path:      path,
		device:    uint64(stat.Dev),
		inode:     stat.Ino,
	}, nil
}

func validateMountpointDirectory(fd int, path string, final bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect FUSE mountpoint directory %q: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("FUSE mountpoint component %q must be a directory", path)
	}
	daemonUID := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != daemonUID {
		return fmt.Errorf(
			"FUSE mountpoint directory %q owner UID is %d, want root or daemon effective UID %d",
			path,
			stat.Uid,
			daemonUID,
		)
	}
	if !final {
		if stat.Mode&0o022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
			return fmt.Errorf("FUSE mountpoint has untrusted writable ancestor %q", path)
		}
		return nil
	}

	mode := os.FileMode(stat.Mode).Perm()
	if mode&0o700 != 0o700 {
		return fmt.Errorf("FUSE mountpoint %q must grant its owner rwx permission", path)
	}
	if mode&mountpointOtherModeMask != 0 {
		return fmt.Errorf("FUSE mountpoint %q must not grant other permissions", path)
	}
	if mode&0o070 != 0 && mode&0o070 != 0o010 {
		return fmt.Errorf("FUSE mountpoint %q group permissions must be 0 or execute-only", path)
	}
	return nil
}

type mountRecord struct {
	mountpoint     string
	filesystemType string
	source         string
	options        map[string]struct{}
}

func (mount mountRecord) scrapeFS() bool {
	return mount.filesystemType == "fuse."+filesystemName &&
		mount.source == filesystemName
}

func (mount mountRecord) ownedScrapeFS() bool {
	if !mount.scrapeFS() {
		return false
	}
	for _, option := range []string{
		fmt.Sprintf("user_id=%d", os.Geteuid()),
		fmt.Sprintf("group_id=%d", os.Getegid()),
		"allow_other",
		"default_permissions",
		"nodev",
		"nosuid",
		"noexec",
	} {
		if !mount.hasOption(option) {
			return false
		}
	}
	return true
}

func (mount mountRecord) hasOption(option string) bool {
	_, ok := mount.options[option]
	return ok
}

func readCurrentMounts() ([]mountRecord, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("open /proc/self/mountinfo: %w", err)
	}
	defer file.Close()

	mounts, err := readMounts(file)
	if err != nil {
		return nil, err
	}
	return mounts, nil
}

func readMounts(reader io.Reader) ([]mountRecord, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var mounts []mountRecord
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if len(fields) < 7 || separator < 6 || separator+3 >= len(fields) {
			return nil, fmt.Errorf("parse /proc/self/mountinfo line %q", scanner.Text())
		}
		mountpoint, err := unescapeMountField(fields[4])
		if err != nil {
			return nil, fmt.Errorf("decode mountpoint %q: %w", fields[4], err)
		}
		source, err := unescapeMountField(fields[separator+2])
		if err != nil {
			return nil, fmt.Errorf("decode mount source %q: %w", fields[separator+2], err)
		}
		options := make(map[string]struct{})
		for _, field := range []string{fields[5], fields[separator+3]} {
			for _, option := range strings.Split(field, ",") {
				options[option] = struct{}{}
			}
		}
		mounts = append(mounts, mountRecord{
			mountpoint:     mountpoint,
			filesystemType: fields[separator+1],
			source:         source,
			options:        options,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	return mounts, nil
}

func unescapeMountField(value string) (string, error) {
	var result strings.Builder
	result.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			result.WriteByte(value[index])
			index++
			continue
		}
		if index+3 >= len(value) {
			return "", errors.New("truncated mountinfo escape")
		}
		decoded, err := strconv.ParseUint(value[index+1:index+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid mountinfo escape %q", value[index:index+4])
		}
		result.WriteByte(byte(decoded))
		index += 4
	}
	return result.String(), nil
}

func clearExistingMount(
	path string,
	mounts []mountRecord,
	unmount func(string, int) error,
) error {
	var existing []mountRecord
	for _, mount := range mounts {
		if mount.mountpoint == path {
			existing = append(existing, mount)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	if len(existing) != 1 {
		return fmt.Errorf("FUSE mountpoint %q has multiple existing mounts", path)
	}
	if !existing[0].ownedScrapeFS() {
		return fmt.Errorf(
			"FUSE mountpoint %q contains an unrelated existing mount (%s from %s)",
			path,
			existing[0].filesystemType,
			existing[0].source,
		)
	}
	if err := unmount(path, 0); err != nil {
		return fmt.Errorf("normally unmount stale ScrapeFS mount %q: %w", path, err)
	}
	return nil
}

func clearStaleMount(path string) error {
	mounts, err := readCurrentMounts()
	if err != nil {
		return err
	}
	if err := clearExistingMount(path, mounts, normalUnmount); err != nil {
		return err
	}
	remaining, err := readCurrentMounts()
	if err != nil {
		return err
	}
	for _, mount := range remaining {
		if mount.mountpoint == path {
			return fmt.Errorf("FUSE mountpoint %q remains mounted after normal unmount", path)
		}
	}
	return nil
}

func normalUnmount(path string, flags int) error {
	if flags != 0 {
		return fmt.Errorf("normal unmount does not accept flags %#x", flags)
	}
	if err := unix.Unmount(path, 0); err == nil {
		return nil
	} else if !errors.Is(err, unix.EPERM) {
		return err
	}

	binary, err := fusermountBinary()
	if err != nil {
		return err
	}
	output, err := exec.Command(binary, "-u", path).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return err
		}
		return fmt.Errorf("%s: %w", message, err)
	}
	return nil
}

func fusermountBinary() (string, error) {
	for _, candidate := range []string{"fusermount3", "fusermount"} {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
		for _, directory := range []string{"/usr/bin", "/bin"} {
			path = filepath.Join(directory, candidate)
			if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
				return path, nil
			}
		}
	}
	return "", errors.New("find fusermount3 or fusermount for normal unmount")
}
