package fuse

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"github.com/zendext/scrape-fs/internal/projection"
	"golang.org/x/sys/unix"
)

func TestMountProjectionFileIntegration(t *testing.T) {
	requireFUSEIntegration(t)

	fixture := newWritableFuseFixture(t)
	service := projection.NewService(fixture.repository)
	sourcePath := filepath.Join(t.TempDir(), "source.mkv")
	sourceContent := []byte("0123456789")
	if err := os.WriteFile(sourcePath, sourceContent, 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	if _, err := service.Create(context.Background(), projection.CreateRequest{
		SourcePath: sourcePath,
		Target:     "jellyfin:/Movies/source.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create source Projection: %v", err)
	}
	conflictSource := filepath.Join(t.TempDir(), "conflict.mkv")
	if err := os.WriteFile(conflictSource, []byte("conflict"), 0o600); err != nil {
		t.Fatalf("WriteFile conflict source: %v", err)
	}
	if _, err := service.Create(context.Background(), projection.CreateRequest{
		SourcePath: conflictSource,
		Target:     "jellyfin:/Movies/existing.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create conflict Projection: %v", err)
	}
	createFuseTestLibrary(t, fixture.repository, "plex", "plex")
	otherSource := filepath.Join(t.TempDir(), "other.mkv")
	if err := os.WriteFile(otherSource, []byte("other"), 0o600); err != nil {
		t.Fatalf("WriteFile other source: %v", err)
	}
	if _, err := service.Create(context.Background(), projection.CreateRequest{
		SourcePath: otherSource,
		Target:     "plex:/Movies/other.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create other Projection: %v", err)
	}

	mountpoint := filepath.Join(t.TempDir(), "mount")
	mounted, err := Mount(
		context.Background(),
		config.FUSE{
			Mountpoint:      mountpoint,
			EntryTimeout:    50 * time.Millisecond,
			AttrTimeout:     50 * time.Millisecond,
			NegativeTimeout: 0,
		},
		fixture.repository,
		fixture.store,
	)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() {
		if err := mounted.Unmount(); err != nil {
			t.Errorf("cleanup Unmount: %v", err)
		}
	})

	sourceTarget := filepath.Join(mountpoint, "jellyfin", "Movies", "source.mkv")
	opened, err := os.Open(sourceTarget)
	if err != nil {
		t.Fatalf("Open Projection: %v", err)
	}
	t.Cleanup(func() {
		opened.Close()
	})
	if offset, err := opened.Seek(4, io.SeekStart); err != nil || offset != 4 {
		t.Fatalf("Seek = %d/%v, want 4/nil", offset, err)
	}
	if got, err := io.ReadAll(opened); err != nil || string(got) != "456789" {
		t.Errorf("read after Seek = %q/%v, want %q/nil", got, err, "456789")
	}

	movies := filepath.Dir(sourceTarget)
	if err := os.Rename(
		sourceTarget,
		filepath.Join(movies, "existing.mkv"),
	); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("Rename over existing Projection error = %v, want EEXIST", err)
	}
	if err := os.Rename(
		sourceTarget,
		filepath.Join(mountpoint, "plex", "Movies", "moved.mkv"),
	); !errors.Is(err, syscall.EXDEV) {
		t.Errorf("cross-Library Rename error = %v, want EXDEV", err)
	}

	renamed := filepath.Join(movies, "renamed.mkv")
	if err := os.Rename(sourceTarget, renamed); err != nil {
		t.Fatalf("Rename Projection: %v", err)
	}
	if _, err := opened.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek after Rename: %v", err)
	}
	if got, err := io.ReadAll(opened); err != nil || string(got) != string(sourceContent) {
		t.Errorf("read after Rename = %q/%v", got, err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatalf("Unlink Projection: %v", err)
	}
	if _, err := opened.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek after Unlink: %v", err)
	}
	if got, err := io.ReadAll(opened); err != nil || string(got) != string(sourceContent) {
		t.Errorf("read after Unlink = %q/%v", got, err)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != string(sourceContent) {
		t.Errorf("source after FUSE mutations = %q/%v", got, err)
	}
}

func TestMountWritableOverlayIntegration(t *testing.T) {
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
			t.Errorf("cleanup database Close: %v", err)
		}
	})
	store, err := objectstore.Open(filepath.Join(root, "objects"), opened)
	if err != nil {
		t.Fatalf("objectstore Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("cleanup objectstore Close: %v", err)
		}
	})
	repository := namespace.NewRepository(opened)
	library := createIntegrationLibrary(t, repository)
	mountpoint := filepath.Join(root, "mount")
	mounted, err := Mount(
		context.Background(),
		config.FUSE{
			Mountpoint:      mountpoint,
			EntryTimeout:    50 * time.Millisecond,
			AttrTimeout:     50 * time.Millisecond,
			NegativeTimeout: 0,
		},
		repository,
		store,
	)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() {
		if err := mounted.Unmount(); err != nil {
			t.Errorf("cleanup Unmount: %v", err)
		}
	})

	movies := filepath.Join(mountpoint, library.Name, "Movies")
	if err := os.Mkdir(movies, 0o750); err != nil {
		t.Fatalf("Mkdir Movies: %v", err)
	}
	path := filepath.Join(movies, "poster.jpg")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o640)
	if err != nil {
		t.Fatalf("OpenFile create Overlay: %v", err)
	}
	t.Cleanup(func() {
		file.Close()
	})
	if _, err := file.WriteAt([]byte("abcdef"), 0); err != nil {
		t.Fatalf("WriteAt initial: %v", err)
	}
	if _, err := file.WriteAt([]byte("XY"), 2); err != nil {
		t.Fatalf("WriteAt random: %v", err)
	}
	appendFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("OpenFile append: %v", err)
	}
	if _, err := appendFile.Write([]byte("-append")); err != nil {
		appendFile.Close()
		t.Fatalf("append Write: %v", err)
	}
	if err := appendFile.Sync(); err != nil {
		appendFile.Close()
		t.Fatalf("append Sync: %v", err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatalf("append Close: %v", err)
	}
	if err := file.Truncate(10); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	atime := time.Unix(11, 12)
	mtime := time.Unix(13, 14)
	if err := os.Chtimes(path, atime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat Overlay: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if info.Mode().Perm() != 0o600 ||
		info.Size() != 10 ||
		stat.Uid != uint32(os.Geteuid()) ||
		stat.Gid != uint32(os.Getegid()) ||
		stat.Atim.Sec != atime.Unix() ||
		stat.Mtim.Sec != mtime.Unix() {
		t.Errorf("Overlay stat = mode %v size %d owner %d:%d atime %v mtime %v",
			info.Mode().Perm(),
			info.Size(),
			stat.Uid,
			stat.Gid,
			stat.Atim,
			stat.Mtim,
		)
	}
	record, err := repository.Child(context.Background(), library.RootNodeID, "Movies")
	if err != nil {
		t.Fatalf("read Movies node: %v", err)
	}
	overlay, err := repository.Child(context.Background(), record.ID, "poster.jpg")
	if err != nil {
		t.Fatalf("read Overlay node: %v", err)
	}
	if overlay.ObjectID == nil {
		t.Fatal("Overlay has no backing object")
	}
	backing, err := store.StatObject(context.Background(), *overlay.ObjectID)
	if err != nil {
		t.Fatalf("StatObject: %v", err)
	}
	if os.FileMode(backing.Mode).Perm() != 0o600 ||
		backing.Uid != uint32(os.Geteuid()) ||
		backing.Gid != uint32(os.Getegid()) {
		t.Errorf("backing metadata = mode %#o owner %d:%d",
			os.FileMode(backing.Mode).Perm(),
			backing.Uid,
			backing.Gid,
		)
	}

	renamed := filepath.Join(movies, "cover.jpg")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	buffer := make([]byte, 10)
	if read, err := file.ReadAt(buffer, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt active unlinked handle: %v", err)
	} else if read != 10 || string(buffer) != "abXYef-app" {
		t.Errorf("active unlinked read = %d/%q", read, buffer)
	}
	if err := os.Remove(movies); err != nil {
		t.Fatalf("Rmdir Movies: %v", err)
	}
}

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

func createIntegrationLibrary(
	t *testing.T,
	repository *namespace.Repository,
) namespace.Library {
	t.Helper()
	var library namespace.Library
	if _, err := repository.Update(
		context.Background(),
		namespace.Operation{
			Name:  "fuse.integration.library",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{"jellyfin"},
				NodeKinds: []namespace.NodeKind{namespace.LibraryRoot},
			},
		},
		func(transaction *namespace.Transaction) error {
			var err error
			library, err = transaction.CreateLibrary(context.Background(), namespace.CreateLibrary{
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
			return err
		},
	); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	return library
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
