package fuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

func TestMountReadOnlyTraversalIntegration(t *testing.T) {
	requireFUSEIntegration(t)

	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	lockPath := filepath.Join(root, "run", "scrapefsd.lock")
	mountpoint := filepath.Join(root, "mount")
	configuration := config.FUSE{
		Mountpoint:      mountpoint,
		EntryTimeout:    50 * time.Millisecond,
		AttrTimeout:     50 * time.Millisecond,
		NegativeTimeout: 0,
	}

	first, err := database.OpenWithOptions(
		context.Background(),
		databasePath,
		database.Options{LockPath: lockPath},
	)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	firstRepository := namespace.NewRepository(first)
	projection := createIntegrationNamespace(t, firstRepository)

	firstMount, err := Mount(
		context.Background(),
		configuration,
		firstRepository,
		&fakeObjectMetadataReader{},
	)
	if err != nil {
		first.Close()
		t.Fatalf("first Mount: %v", err)
	}
	t.Cleanup(func() {
		if err := firstMount.Unmount(); err != nil {
			t.Errorf("cleanup first Unmount: %v", err)
		}
		if err := first.Close(); err != nil {
			t.Errorf("cleanup first Close: %v", err)
		}
	})

	assertIntegrationTraversal(t, mountpoint, projection)
	firstInode := statInode(t, filepath.Join(mountpoint, "jellyfin", "Movies", projection.Name))
	assertIntegrationMutationProtection(t, mountpoint)
	assertIntegrationMountFlags(t, mountpoint)

	if err := firstMount.Unmount(); err != nil {
		first.Close()
		t.Fatalf("first Unmount: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := database.OpenWithOptions(
		context.Background(),
		databasePath,
		database.Options{LockPath: lockPath},
	)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("cleanup second Close: %v", err)
		}
	})

	secondMount, err := Mount(
		context.Background(),
		configuration,
		namespace.NewRepository(second),
		&fakeObjectMetadataReader{},
	)
	if err != nil {
		t.Fatalf("second Mount: %v", err)
	}
	t.Cleanup(func() {
		if err := secondMount.Unmount(); err != nil {
			t.Errorf("cleanup second Unmount: %v", err)
		}
	})

	secondInode := statInode(
		t,
		filepath.Join(mountpoint, "jellyfin", "Movies", projection.Name),
	)
	if secondInode != firstInode || secondInode != uint64(projection.ID) {
		t.Errorf(
			"inode after restart = %d, before = %d, persisted node ID = %d",
			secondInode,
			firstInode,
			projection.ID,
		)
	}
}

func TestAnchoredMountIgnoresPreMountPathReplacementIntegration(t *testing.T) {
	requireFUSEIntegration(t)

	root := t.TempDir()
	opened, err := database.OpenWithOptions(
		context.Background(),
		filepath.Join(root, "state", "scrapefs.db"),
		database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")},
	)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("cleanup Close: %v", err)
		}
	})

	repository := namespace.NewRepository(opened)
	rootNode, err := newRoot(
		context.Background(),
		repository,
		&fakeObjectMetadataReader{},
	)
	if err != nil {
		t.Fatalf("newRoot: %v", err)
	}

	parent := filepath.Join(root, "parent")
	mountpoint := filepath.Join(parent, "mount")
	prepared, err := prepareMountpointDirectory(mountpoint)
	if err != nil {
		t.Fatalf("prepareMountpointDirectory: %v", err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Errorf("cleanup prepared Close: %v", err)
		}
	})
	if err := prepared.verifyPath(); err != nil {
		t.Fatalf("verifyPath: %v", err)
	}

	originalParent := filepath.Join(root, "original")
	if err := os.Rename(parent, originalParent); err != nil {
		t.Fatalf("Rename parent: %v", err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("Mkdir replacement parent: %v", err)
	}
	if err := os.Mkdir(mountpoint, mountpointMode); err != nil {
		t.Fatalf("Mkdir replacement mountpoint: %v", err)
	}

	options := filesystemOptions(config.FUSE{
		EntryTimeout:    50 * time.Millisecond,
		AttrTimeout:     50 * time.Millisecond,
		NegativeTimeout: 0,
	})
	server, err := mountPrepared(prepared, rootNode, &options, mountNodeFS)
	if err != nil {
		t.Fatalf("mountPrepared: %v", err)
	}
	t.Cleanup(func() {
		if err := unmountPrepared(prepared, server); err != nil {
			t.Errorf("cleanup unmountPrepared: %v", err)
		}
	})

	anchoredPath := filepath.Join(originalParent, "mount")
	mounts, err := readCurrentMounts()
	if err != nil {
		t.Fatalf("readCurrentMounts: %v", err)
	}
	var anchored *mountRecord
	for index := range mounts {
		switch mounts[index].mountpoint {
		case mountpoint:
			t.Errorf("replacement path %q was mounted", mountpoint)
		case anchoredPath:
			anchored = &mounts[index]
		}
	}
	if anchored == nil || !anchored.scrapeFS() {
		t.Fatalf("anchored mount = %#v, want ScrapeFS at %q", anchored, anchoredPath)
	}
	for _, option := range []string{
		"allow_other",
		"default_permissions",
		"nodev",
		"nosuid",
		"noexec",
	} {
		if !anchored.hasOption(option) {
			t.Errorf("anchored mount options missing %q: %#v", option, anchored)
		}
	}
	if entries, err := os.ReadDir(mountpoint); err != nil {
		t.Fatalf("ReadDir replacement mountpoint: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("replacement mountpoint entries = %v, want empty underlying directory", entries)
	}

	if err := unmountPrepared(prepared, server); err != nil {
		t.Fatalf("unmountPrepared: %v", err)
	}
	mounts, err = readCurrentMounts()
	if err != nil {
		t.Fatalf("readCurrentMounts after unmount: %v", err)
	}
	for _, mount := range mounts {
		if mount.mountpoint == anchoredPath {
			t.Fatalf("anchored mount remains after normal unmount: %#v", mount)
		}
	}
}

func requireFUSEIntegration(t *testing.T) {
	t.Helper()

	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("real FUSE integration unavailable: /dev/fuse: %v", err)
	}
	if os.Geteuid() != 0 && !hasEffectiveCapability(t, unix.CAP_SYS_ADMIN) {
		t.Skip("real FUSE integration unavailable: descriptor-anchored direct mount requires root or effective CAP_SYS_ADMIN")
	}
	if _, err := os.Stat("/proc/self/mountinfo"); err != nil {
		t.Skipf("real FUSE integration unavailable: /proc/self/mountinfo: %v", err)
	}
}

func hasEffectiveCapability(t *testing.T, capability int) bool {
	t.Helper()

	header := unix.CapUserHeader{
		Version: unix.LINUX_CAPABILITY_VERSION_3,
	}
	var data [2]unix.CapUserData
	if err := unix.Capget(&header, &data[0]); err != nil {
		t.Skipf("real FUSE integration unavailable: read effective capabilities: %v", err)
	}
	index := capability / 32
	mask := uint32(1) << uint(capability%32)
	return data[index].Effective&mask != 0
}

func createIntegrationNamespace(
	t *testing.T,
	repository *namespace.Repository,
) namespace.Node {
	t.Helper()

	ctx := context.Background()
	var projection namespace.Node
	_, err := repository.Update(ctx, namespace.Operation{
		Name:  "fuse.integration",
		Actor: namespace.Actor{Kind: namespace.ActorSystem},
		Details: namespace.OperationDetails{
			Paths: []string{
				"jellyfin",
				"jellyfin/Movies",
				"jellyfin/Movies/Dune.mkv",
			},
			NodeKinds: []namespace.NodeKind{
				namespace.LibraryRoot,
				namespace.Directory,
				namespace.ProjectionFile,
			},
		},
	}, func(transaction *namespace.Transaction) error {
		library, err := transaction.CreateLibrary(ctx, namespace.CreateLibrary{
			Name:           "jellyfin",
			ProfileName:    "jellyfin",
			ProfileVersion: 1,
			Creator:        namespace.Actor{Kind: namespace.ActorSystem},
			UID:            uint32(os.Geteuid()),
			GID:            uint32(os.Getegid()),
			RootMode:       0o775,
			DirectoryMode:  0o775,
			ProjectionMode: 0o444,
			OverlayMode:    0o664,
		})
		if err != nil {
			return err
		}
		movies, err := transaction.CreateNode(ctx, namespace.CreateNode{
			ParentID: library.RootNodeID,
			Name:     "Movies",
			Kind:     namespace.Directory,
			Creator:  namespace.Actor{Kind: namespace.ActorSystem},
			Attributes: namespace.Attributes{
				Mode: library.DirectoryMode,
				UID:  library.UID,
				GID:  library.GID,
			},
		})
		if err != nil {
			return err
		}
		projection, err = transaction.CreateNode(ctx, namespace.CreateNode{
			ParentID: movies.ID,
			Name:     "Dune.mkv",
			Kind:     namespace.ProjectionFile,
			Source: &namespace.ProjectionSource{
				Path:          "/missing/Dune.mkv",
				DeviceID:      1,
				Inode:         2,
				Size:          42,
				AtimeNS:       1_700_000_000_000_000_001,
				MtimeNS:       1_700_000_000_000_000_002,
				SourceCtimeNS: 1_700_000_000_000_000_003,
				VerifiedAtNS:  1_700_000_000_000_000_004,
			},
			Creator: namespace.Actor{Kind: namespace.ActorSystem},
			Attributes: namespace.Attributes{
				Mode: library.ProjectionMode,
				UID:  library.UID,
				GID:  library.GID,
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("create integration namespace: %v", err)
	}
	return projection
}

func assertIntegrationTraversal(
	t *testing.T,
	mountpoint string,
	projection namespace.Node,
) {
	t.Helper()

	rootEntries, err := os.ReadDir(mountpoint)
	if err != nil {
		t.Fatalf("ReadDir root: %v", err)
	}
	if len(rootEntries) != 1 || rootEntries[0].Name() != "jellyfin" || !rootEntries[0].IsDir() {
		t.Fatalf("root entries = %#v", rootEntries)
	}

	path := filepath.Join(mountpoint, "jellyfin", "Movies", projection.Name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat projection: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 || info.Size() != 42 {
		t.Errorf("projection info = mode %v size %d", info.Mode(), info.Size())
	}
}

func assertIntegrationMutationProtection(t *testing.T, mountpoint string) {
	t.Helper()

	if err := os.Mkdir(filepath.Join(mountpoint, "new-library"), 0o755); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Mkdir GlobalRoot error = %v, want EPERM", err)
	}
	if err := os.Remove(filepath.Join(mountpoint, "jellyfin")); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Remove LibraryRoot error = %v, want EPERM", err)
	}
	if err := os.Rename(
		filepath.Join(mountpoint, "jellyfin"),
		filepath.Join(mountpoint, "renamed"),
	); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Rename LibraryRoot error = %v, want EPERM", err)
	}
	if err := os.Chmod(filepath.Join(mountpoint, "jellyfin"), 0o700); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Chmod LibraryRoot error = %v, want EPERM", err)
	}
}

func assertIntegrationMountFlags(t *testing.T, mountpoint string) {
	t.Helper()

	mounts, err := readCurrentMounts()
	if err != nil {
		t.Fatalf("readCurrentMounts: %v", err)
	}
	for _, mount := range mounts {
		if mount.mountpoint != mountpoint {
			continue
		}
		if !mount.ownedScrapeFS() {
			t.Errorf("mount is not recognized as daemon-owned ScrapeFS: %#v", mount)
		}
		for _, option := range []string{
			"allow_other",
			"default_permissions",
			"nodev",
			"nosuid",
			"noexec",
		} {
			if !mount.hasOption(option) {
				t.Errorf("mount options missing %q: %#v", option, mount)
			}
		}
		return
	}
	t.Fatalf("mountpoint %q not found in mountinfo", mountpoint)
}

func statInode(t *testing.T, path string) uint64 {
	t.Helper()

	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		t.Fatalf("Stat %q: %v", path, err)
	}
	return stat.Ino
}
