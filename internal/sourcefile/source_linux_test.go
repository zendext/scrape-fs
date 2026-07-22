package sourcefile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenReturnsReadOnlyDescriptorAndMetadata(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "media")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	path := filepath.Join(directory, "movie.mkv")
	content := []byte("original source")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	opened, err := Open(directory + "/./movie.mkv")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer opened.Close()

	if opened.Path != path {
		t.Errorf("Path = %q, want %q", opened.Path, path)
	}

	flags, err := unix.FcntlInt(opened.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("FcntlInt(F_GETFL): %v", err)
	}
	if accessMode := flags & unix.O_ACCMODE; accessMode != unix.O_RDONLY {
		t.Errorf("descriptor access mode = %#x, want O_RDONLY", accessMode)
	}

	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if opened.DeviceID != int64(stat.Dev) ||
		opened.Inode != int64(stat.Ino) ||
		opened.Size != stat.Size ||
		opened.AtimeNS != stat.Atim.Nano() ||
		opened.MtimeNS != stat.Mtim.Nano() ||
		opened.CtimeNS != stat.Ctim.Nano() {
		t.Errorf("metadata = %#v, want stat %#v", opened.Metadata, stat)
	}

	originalPath := path + ".original"
	if err := os.Rename(path, originalPath); err != nil {
		t.Fatalf("Rename original: %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("Write replacement: %v", err)
	}

	got, err := io.ReadAll(opened)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("read content = %q, want %q", got, content)
	}
}

func TestOpenRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "child"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	tests := map[string]struct {
		path        string
		errorDetail string
	}{
		"relative": {
			path:        "source",
			errorDetail: "absolute",
		},
		"parent traversal": {
			path:        filepath.Join(root, "child") + "/../source",
			errorDetail: "..",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			opened, err := Open(test.path)
			if opened != nil {
				opened.Close()
			}
			if err == nil {
				t.Fatal("Open succeeded")
			}
			if !strings.Contains(err.Error(), test.errorDetail) {
				t.Errorf("Open error = %q, want detail %q", err, test.errorDetail)
			}
		})
	}
}

func TestOpenRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatalf("Mkdir real directory: %v", err)
	}
	realPath := filepath.Join(realDirectory, "source")
	if err := os.WriteFile(realPath, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	finalLink := filepath.Join(root, "source-link")
	if err := os.Symlink(realPath, finalLink); err != nil {
		t.Fatalf("Symlink final component: %v", err)
	}
	directoryLink := filepath.Join(root, "directory-link")
	if err := os.Symlink(realDirectory, directoryLink); err != nil {
		t.Fatalf("Symlink directory component: %v", err)
	}

	for name, path := range map[string]string{
		"final component":     finalLink,
		"directory component": filepath.Join(directoryLink, "source"),
	} {
		t.Run(name, func(t *testing.T) {
			opened, err := Open(path)
			if opened != nil {
				opened.Close()
			}
			if err == nil {
				t.Fatal("Open succeeded")
			}
		})
	}
}

func TestOpenRejectsMagicLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	target, err := os.Open(path)
	if err != nil {
		t.Fatalf("os.Open: %v", err)
	}
	defer target.Close()

	magicPath := filepath.Join(
		"/proc",
		strconv.Itoa(os.Getpid()),
		"fd",
		strconv.FormatUint(uint64(target.Fd()), 10),
	)
	opened, err := Open(magicPath)
	if opened != nil {
		opened.Close()
	}
	if err == nil {
		t.Fatal("Open succeeded")
	}
}

func TestOpenRejectsNonRegularFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "source.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	tests := map[string]string{
		"directory": t.TempDir(),
		"FIFO":      fifo,
	}
	if _, err := os.Stat("/dev/null"); err == nil {
		tests["character device"] = "/dev/null"
	}

	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			opened, err := Open(path)
			if opened != nil {
				opened.Close()
			}
			if err == nil {
				t.Fatal("Open succeeded")
			}
			if !strings.Contains(err.Error(), "regular file") {
				t.Errorf("Open error = %q, want regular file detail", err)
			}
		})
	}
}

func TestOpenVerifiedChecksStrictIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	opened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	identity := opened.Identity
	if err := opened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tests := map[string]Identity{
		"device ID": identity,
		"inode":     identity,
		"size":      identity,
		"mtime":     identity,
	}
	deviceMismatch := tests["device ID"]
	deviceMismatch.DeviceID ^= 1
	tests["device ID"] = deviceMismatch
	inodeMismatch := tests["inode"]
	inodeMismatch.Inode ^= 1
	tests["inode"] = inodeMismatch
	sizeMismatch := tests["size"]
	sizeMismatch.Size ^= 1
	tests["size"] = sizeMismatch
	mtimeMismatch := tests["mtime"]
	mtimeMismatch.MtimeNS ^= 1
	tests["mtime"] = mtimeMismatch

	for name, expected := range tests {
		t.Run(name, func(t *testing.T) {
			verified, err := OpenVerified(path, expected)
			if verified != nil {
				verified.Close()
			}
			if !errors.Is(err, unix.ESTALE) {
				t.Fatalf("OpenVerified error = %v, want ESTALE", err)
			}
		})
	}

	verified, err := OpenVerified(path, identity)
	if err != nil {
		t.Fatalf("OpenVerified matching identity: %v", err)
	}
	if err := verified.Close(); err != nil {
		t.Fatalf("Close verified: %v", err)
	}
}

func TestOpenVerifiedDetectsSamePathReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile original: %v", err)
	}
	opened, err := Open(path)
	if err != nil {
		t.Fatalf("Open original: %v", err)
	}
	identity := opened.Identity
	if err := opened.Close(); err != nil {
		t.Fatalf("Close original: %v", err)
	}

	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile replacement: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("Rename replacement: %v", err)
	}

	verified, err := OpenVerified(path, identity)
	if verified != nil {
		verified.Close()
	}
	if !errors.Is(err, unix.ESTALE) {
		t.Fatalf("OpenVerified error = %v, want ESTALE", err)
	}
}

func TestOpenFailsClosedWhenOpenat2IsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for name, unavailableError := range map[string]error{
		"missing syscall":       unix.ENOSYS,
		"unsupported semantics": unix.EINVAL,
		"blocked syscall":       unix.EPERM,
	} {
		t.Run(name, func(t *testing.T) {
			opened, err := openWithOpenat2(
				path,
				func(rootFD int, relativePath string, how *unix.OpenHow) (int, error) {
					if rootFD < 0 {
						t.Errorf("root fd = %d, want open descriptor", rootFD)
					}
					if strings.HasPrefix(relativePath, "/") {
						t.Errorf("openat2 path = %q, want root-relative path", relativePath)
					}
					if how.Resolve != requiredResolveFlags {
						t.Errorf("resolve flags = %#x, want %#x", how.Resolve, requiredResolveFlags)
					}
					if accessMode := int(how.Flags) & unix.O_ACCMODE; accessMode != unix.O_RDONLY {
						t.Errorf("open flags access mode = %#x, want O_RDONLY", accessMode)
					}
					return -1, unavailableError
				},
			)
			if opened != nil {
				opened.Close()
			}
			if !errors.Is(err, ErrOpenat2Unavailable) {
				t.Fatalf("openWithOpenat2 error = %v, want ErrOpenat2Unavailable", err)
			}
			if !strings.Contains(err.Error(), "Linux 5.6") {
				t.Errorf("openWithOpenat2 error = %q, want actionable kernel requirement", err)
			}
		})
	}
}
