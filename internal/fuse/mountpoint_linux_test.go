package fuse

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/zendext/scrape-fs/internal/namespace"
)

func TestPrepareMountpointCreatesFailClosedEmptyDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent", "mount")

	prepared, err := prepareMountpointDirectory(path)
	if err != nil {
		t.Fatalf("prepareMountpointDirectory: %v", err)
	}
	defer prepared.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != mountpointMode {
		t.Errorf("mode = %#o, want %#o", got, mountpointMode)
	}
}

func TestPreparedMountpointRejectsPathReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mount")
	prepared, err := prepareMountpointDirectory(path)
	if err != nil {
		t.Fatalf("prepareMountpointDirectory: %v", err)
	}
	defer prepared.Close()

	original := filepath.Join(root, "original")
	if err := os.Rename(path, original); err != nil {
		t.Fatalf("Rename mountpoint: %v", err)
	}
	if err := os.Mkdir(path, mountpointMode); err != nil {
		t.Fatalf("Mkdir replacement: %v", err)
	}
	if err := prepared.verifyPath(); err == nil ||
		!strings.Contains(err.Error(), "replaced") {
		t.Fatalf("verifyPath error = %v, want replacement rejection", err)
	}
}

func TestPreparedMountpointRejectsAncestorSymlinkReplacement(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	path := filepath.Join(parent, "mount")
	prepared, err := prepareMountpointDirectory(path)
	if err != nil {
		t.Fatalf("prepareMountpointDirectory: %v", err)
	}
	defer prepared.Close()

	original := filepath.Join(root, "original")
	if err := os.Rename(parent, original); err != nil {
		t.Fatalf("Rename parent: %v", err)
	}
	if err := os.Symlink(original, parent); err != nil {
		t.Fatalf("Symlink parent: %v", err)
	}
	if err := prepared.verifyPath(); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("verifyPath error = %v, want ancestor symlink rejection", err)
	}
}

func TestMountPreparedUsesHeldDirectoryAfterPathReplacement(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	path := filepath.Join(parent, "mount")
	prepared, err := prepareMountpointDirectory(path)
	if err != nil {
		t.Fatalf("prepareMountpointDirectory: %v", err)
	}
	defer prepared.Close()
	if err := prepared.verifyPath(); err != nil {
		t.Fatalf("verifyPath: %v", err)
	}

	original := filepath.Join(root, "original")
	if err := os.Rename(parent, original); err != nil {
		t.Fatalf("Rename parent: %v", err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("Mkdir replacement parent: %v", err)
	}
	if err := os.Mkdir(path, mountpointMode); err != nil {
		t.Fatalf("Mkdir replacement mountpoint: %v", err)
	}

	rootNode := &node{
		reader: &fakeNamespaceReader{},
		record: testNode(1, 0, "", namespace.GlobalRoot),
	}
	var target string
	_, err = mountPrepared(
		prepared,
		rootNode,
		&gofs.Options{},
		func(got string, _ gofs.InodeEmbedder, _ *gofs.Options) (mountedServer, error) {
			target = got
			var stat syscall.Stat_t
			if err := syscall.Stat(got, &stat); err != nil {
				return nil, err
			}
			if uint64(stat.Dev) != prepared.device || stat.Ino != prepared.inode {
				t.Errorf(
					"mount target inode = %d:%d, want held %d:%d",
					stat.Dev,
					stat.Ino,
					prepared.device,
					prepared.inode,
				)
			}
			return fakeMountedServer{}, nil
		},
	)
	if err != nil {
		t.Fatalf("mountPrepared: %v", err)
	}
	if !strings.HasPrefix(target, "/proc/self/fd/") {
		t.Errorf("mount target = %q, want descriptor path", target)
	}

	var replacement syscall.Stat_t
	if err := syscall.Stat(path, &replacement); err != nil {
		t.Fatalf("Stat replacement: %v", err)
	}
	if uint64(replacement.Dev) == prepared.device && replacement.Ino == prepared.inode {
		t.Fatal("replacement unexpectedly has held mountpoint identity")
	}
}

type fakeMountedServer struct{}

func (fakeMountedServer) Unmount() error {
	return nil
}

func TestPrepareMountpointRejectsUnsafePaths(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, mountpointMode); err != nil {
			t.Fatalf("Mkdir target: %v", err)
		}
		path := filepath.Join(root, "mount")
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		if _, err := prepareMountpointDirectory(path); err == nil ||
			!strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error = %v, want symlink rejection", err)
		}
	})

	t.Run("non-empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mount")
		if err := os.Mkdir(path, mountpointMode); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "hidden"), nil, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if _, err := prepareMountpointDirectory(path); err == nil ||
			!strings.Contains(err.Error(), "empty") {
			t.Fatalf("error = %v, want non-empty rejection", err)
		}
	})

	t.Run("world-accessible", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mount")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}

		if _, err := prepareMountpointDirectory(path); err == nil ||
			!strings.Contains(err.Error(), "other permissions") {
			t.Fatalf("error = %v, want fail-closed mode rejection", err)
		}
	})
}

func TestReadMountsDecodesPathsAndIdentifiesScrapeFS(t *testing.T) {
	input := strings.NewReader(
		"10 1 0:1 / /tmp rw - tmpfs tmpfs rw\n" +
			"11 10 0:2 / /tmp/scrape\\040fs rw,nosuid,nodev,noexec - fuse.scrapefs scrapefs rw,user_id=1000,group_id=1000,allow_other,default_permissions\n",
	)
	mounts, err := readMounts(input)
	if err != nil {
		t.Fatalf("readMounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mount count = %d, want 2", len(mounts))
	}
	mount := mounts[1]
	if mount.mountpoint != "/tmp/scrape fs" ||
		mount.filesystemType != "fuse.scrapefs" ||
		mount.source != "scrapefs" ||
		!mount.hasOption("nosuid") ||
		!mount.hasOption("nodev") ||
		!mount.hasOption("noexec") ||
		!mount.hasOption("allow_other") ||
		!mount.hasOption("default_permissions") {
		t.Errorf("mount = %#v", mount)
	}
	if !mount.scrapeFS() {
		t.Errorf("scrapeFS = false for %#v", mount)
	}
}

func TestClearExistingMountFailsClosed(t *testing.T) {
	t.Run("unknown mount", func(t *testing.T) {
		unmounted := false
		err := clearExistingMount(
			"/mnt/scrapefs",
			[]mountRecord{{
				mountpoint:     "/mnt/scrapefs",
				filesystemType: "fuse.unknown",
				source:         "unknown",
			}},
			func(string, int) error {
				unmounted = true
				return nil
			},
		)
		if err == nil || !strings.Contains(err.Error(), "unrelated") {
			t.Fatalf("error = %v, want unrelated mount rejection", err)
		}
		if unmounted {
			t.Fatal("unknown mount was unmounted")
		}
	})

	t.Run("stale ScrapeFS mount", func(t *testing.T) {
		var path string
		var flags int
		err := clearExistingMount(
			"/mnt/scrapefs",
			[]mountRecord{{
				mountpoint:     "/mnt/scrapefs",
				filesystemType: "fuse.scrapefs",
				source:         "scrapefs",
			}},
			func(gotPath string, gotFlags int) error {
				path = gotPath
				flags = gotFlags
				return nil
			},
		)
		if err != nil {
			t.Fatalf("clearExistingMount: %v", err)
		}
		if path != "/mnt/scrapefs" || flags != 0 {
			t.Errorf("unmount = %q flags %#x, want normal unmount", path, flags)
		}
	})

	t.Run("busy stale mount", func(t *testing.T) {
		err := clearExistingMount(
			"/mnt/scrapefs",
			[]mountRecord{{
				mountpoint:     "/mnt/scrapefs",
				filesystemType: "fuse.scrapefs",
				source:         "scrapefs",
			}},
			func(string, int) error {
				return syscall.EBUSY
			},
		)
		if !errors.Is(err, syscall.EBUSY) {
			t.Fatalf("error = %v, want EBUSY", err)
		}
	})
}
