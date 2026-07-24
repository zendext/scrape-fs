package fuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"golang.org/x/sys/unix"
)

func TestOverlayHandleSupportsRandomIOAndLifecycle(t *testing.T) {
	raw := openTestObjectFile(t, unix.O_RDWR)
	handle := &overlayHandle{file: raw, flags: unix.O_RDWR}

	if written, errno := handle.Write(context.Background(), []byte("abcdef"), 0); errno != 0 || written != 6 {
		t.Fatalf("first Write = %d/%v, want 6/OK", written, errno)
	}
	if written, errno := handle.Write(context.Background(), []byte("XY"), 2); errno != 0 || written != 2 {
		t.Fatalf("random Write = %d/%v, want 2/OK", written, errno)
	}
	result, errno := handle.Read(context.Background(), make([]byte, 6), 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	content, status := result.Bytes(make([]byte, 6))
	result.Done()
	if status != gofuse.OK || string(content) != "abXYef" {
		t.Errorf("Read = %q/%v, want %q/OK", content, status, "abXYef")
	}
	if errno := handle.Flush(context.Background()); errno != 0 {
		t.Errorf("Flush errno = %v", errno)
	}
	if errno := handle.Fsync(context.Background(), 0); errno != 0 {
		t.Errorf("Fsync errno = %v", errno)
	}
	if errno := handle.Fsync(context.Background(), 1); errno != 0 {
		t.Errorf("Fdatasync errno = %v", errno)
	}
	if errno := handle.Allocate(context.Background(), 0, 4096, 0); errno != syscall.EOPNOTSUPP {
		t.Errorf("Allocate errno = %v, want EOPNOTSUPP", errno)
	}
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Errorf("Release errno = %v", errno)
	}
	if errno := handle.Fsync(context.Background(), 0); errno != syscall.EBADF {
		t.Errorf("Fsync after Release errno = %v, want EBADF", errno)
	}
}

func TestOverlayHandleAppendWritesWholeRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	first := openObjectFilePath(t, path, unix.O_WRONLY|unix.O_APPEND)
	second := openObjectFilePath(t, path, unix.O_WRONLY|unix.O_APPEND)
	firstHandle := &overlayHandle{file: first, flags: unix.O_WRONLY | unix.O_APPEND}
	secondHandle := &overlayHandle{file: second, flags: unix.O_WRONLY | unix.O_APPEND}
	t.Cleanup(func() {
		firstHandle.Release(context.Background())
		secondHandle.Release(context.Background())
	})

	const writes = 100
	var wait sync.WaitGroup
	for _, writer := range []struct {
		handle *overlayHandle
		data   []byte
	}{
		{handle: firstHandle, data: []byte("aaaaaaaa\n")},
		{handle: secondHandle, data: []byte("bbbbbbbb\n")},
	} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range writes {
				if written, errno := writer.handle.Write(
					context.Background(),
					writer.data,
					0,
				); errno != 0 || written != uint32(len(writer.data)) {
					t.Errorf("append Write = %d/%v", written, errno)
					return
				}
			}
		}()
	}
	wait.Wait()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) != writes*2 {
		t.Fatalf("line count = %d, want %d", len(lines), writes*2)
	}
	for _, line := range lines {
		if line != "aaaaaaaa" && line != "bbbbbbbb" {
			t.Fatalf("torn append record %q", line)
		}
	}
}

func TestWritableFuseDirectoryAndOverlayLifecycle(t *testing.T) {
	fixture := newWritableFuseFixture(t)
	ctx := fuseCallerContext(1234, 5678)

	moviesInode, errno := fixture.libraryRoot.Mkdir(
		ctx,
		"Movies",
		0o750,
		&gofuse.EntryOut{},
	)
	if errno != 0 {
		t.Fatalf("Mkdir errno = %v", errno)
	}
	movies := moviesInode.Operations().(*node)
	persistedMovies, err := fixture.repository.Child(
		context.Background(),
		fixture.library.RootNodeID,
		"Movies",
	)
	if err != nil {
		t.Fatalf("read created Directory: %v", err)
	}
	if persistedMovies.Attributes != (namespace.Attributes{Mode: 0o750, UID: 1234, GID: 5678}) {
		t.Errorf("Directory attributes = %#v", persistedMovies.Attributes)
	}
	var directoryAttr gofuse.AttrOut
	if errno := movies.Setattr(ctx, nil, &gofuse.SetAttrIn{
		SetAttrInCommon: gofuse.SetAttrInCommon{
			Valid: gofuse.FATTR_MODE |
				gofuse.FATTR_UID |
				gofuse.FATTR_GID |
				gofuse.FATTR_ATIME |
				gofuse.FATTR_MTIME,
			Mode:      0o700,
			Owner:     gofuse.Owner{Uid: 1300, Gid: 1301},
			Atime:     7,
			Atimensec: 8,
			Mtime:     9,
			Mtimensec: 10,
		},
	}, &directoryAttr); errno != 0 {
		t.Fatalf("Directory Setattr errno = %v", errno)
	}
	if directoryAttr.Mode != syscall.S_IFDIR|0o700 ||
		directoryAttr.Uid != 1300 ||
		directoryAttr.Gid != 1301 ||
		directoryAttr.Atime != 7 ||
		directoryAttr.Atimensec != 8 ||
		directoryAttr.Mtime != 9 ||
		directoryAttr.Mtimensec != 10 {
		t.Errorf("updated Directory attrs = %#v", directoryAttr.Attr)
	}

	fileInode, fileHandle, _, errno := movies.Create(
		ctx,
		"poster.jpg",
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL,
		0o640,
		&gofuse.EntryOut{},
	)
	if errno != 0 {
		t.Fatalf("Create errno = %v", errno)
	}
	fileNode := fileInode.Operations().(*node)
	handle := fileHandle.(*overlayHandle)
	t.Cleanup(func() {
		handle.Release(context.Background())
	})
	if written, errno := handle.Write(ctx, []byte("poster-data"), 0); errno != 0 || written != 11 {
		t.Fatalf("Write = %d/%v, want 11/OK", written, errno)
	}
	if written, errno := handle.Write(ctx, []byte("ART"), 7); errno != 0 || written != 3 {
		t.Fatalf("random Write = %d/%v, want 3/OK", written, errno)
	}

	mode := uint32(0o600)
	var attr gofuse.AttrOut
	if errno := fileNode.Setattr(ctx, handle, &gofuse.SetAttrIn{
		SetAttrInCommon: gofuse.SetAttrInCommon{
			Valid: gofuse.FATTR_MODE |
				gofuse.FATTR_UID |
				gofuse.FATTR_GID |
				gofuse.FATTR_SIZE |
				gofuse.FATTR_ATIME |
				gofuse.FATTR_MTIME,
			Mode:      mode,
			Owner:     gofuse.Owner{Uid: 2000, Gid: 2001},
			Size:      10,
			Atime:     11,
			Atimensec: 12,
			Mtime:     13,
			Mtimensec: 14,
		},
	}, &attr); errno != 0 {
		t.Fatalf("Setattr errno = %v", errno)
	}
	if attr.Size != 10 ||
		attr.Mode != syscall.S_IFREG|mode ||
		attr.Uid != 2000 ||
		attr.Gid != 2001 ||
		attr.Atime != 11 ||
		attr.Atimensec != 12 ||
		attr.Mtime != 13 ||
		attr.Mtimensec != 14 {
		t.Errorf("updated attrs = %#v", attr.Attr)
	}
	var backing unix.Stat_t
	if err := unix.Fstat(int(handle.file.Fd()), &backing); err != nil {
		t.Fatalf("Fstat backing object: %v", err)
	}
	if os.FileMode(backing.Mode).Perm() != 0o600 ||
		backing.Uid != uint32(os.Geteuid()) ||
		backing.Gid != uint32(os.Getegid()) {
		t.Errorf(
			"backing metadata = mode %#o owner %d:%d",
			os.FileMode(backing.Mode).Perm(),
			backing.Uid,
			backing.Gid,
		)
	}

	if errno := movies.Rename(ctx, "poster.jpg", movies, "cover.jpg", 0); errno != 0 {
		t.Fatalf("Rename errno = %v", errno)
	}
	if _, err := fixture.repository.Child(context.Background(), movies.record.ID, "poster.jpg"); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("old path error = %v, want ErrNotFound", err)
	}
	if _, err := fixture.repository.Child(context.Background(), movies.record.ID, "cover.jpg"); err != nil {
		t.Fatalf("new path: %v", err)
	}
	if errno := fixture.libraryRoot.Rmdir(ctx, "Movies"); errno != syscall.ENOTEMPTY {
		t.Errorf("Rmdir non-empty errno = %v, want ENOTEMPTY", errno)
	}
	if errno := movies.Unlink(ctx, "cover.jpg"); errno != 0 {
		t.Fatalf("Unlink errno = %v", errno)
	}
	if _, err := fixture.repository.Child(context.Background(), movies.record.ID, "cover.jpg"); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("unlinked path error = %v, want ErrNotFound", err)
	}
	if written, errno := handle.Write(ctx, []byte("X"), 0); errno != 0 || written != 1 {
		t.Fatalf("Write active unlinked handle = %d/%v, want 1/OK", written, errno)
	}
	var unlinkedAttr gofuse.AttrOut
	if errno := fileNode.Setattr(ctx, handle, &gofuse.SetAttrIn{
		SetAttrInCommon: gofuse.SetAttrInCommon{
			Valid: gofuse.FATTR_MODE |
				gofuse.FATTR_UID |
				gofuse.FATTR_GID |
				gofuse.FATTR_SIZE |
				gofuse.FATTR_ATIME |
				gofuse.FATTR_MTIME,
			Mode:      0o604,
			Owner:     gofuse.Owner{Uid: 3000, Gid: 3001},
			Size:      6,
			Atime:     21,
			Atimensec: 22,
			Mtime:     23,
			Mtimensec: 24,
		},
	}, &unlinkedAttr); errno != 0 {
		t.Fatalf("Setattr active unlinked handle errno = %v", errno)
	}
	if unlinkedAttr.Size != 6 ||
		unlinkedAttr.Mode != syscall.S_IFREG|0o604 ||
		unlinkedAttr.Uid != 3000 ||
		unlinkedAttr.Gid != 3001 ||
		unlinkedAttr.Atime != 21 ||
		unlinkedAttr.Atimensec != 22 ||
		unlinkedAttr.Mtime != 23 ||
		unlinkedAttr.Mtimensec != 24 {
		t.Errorf("active unlinked setattr attrs = %#v", unlinkedAttr.Attr)
	}
	result, errno := handle.Read(ctx, make([]byte, 6), 0)
	if errno != 0 {
		t.Fatalf("Read active unlinked handle errno = %v", errno)
	}
	content, status := result.Bytes(make([]byte, 6))
	result.Done()
	if status != gofuse.OK || string(content) != "Xoster" {
		t.Errorf("active handle content = %q/%v", content, status)
	}
	if errno := fileNode.Getattr(ctx, handle, &unlinkedAttr); errno != 0 {
		t.Fatalf("Getattr active unlinked handle errno = %v", errno)
	}
	if unlinkedAttr.Size != 6 ||
		unlinkedAttr.Mode != syscall.S_IFREG|0o604 ||
		unlinkedAttr.Uid != 3000 ||
		unlinkedAttr.Gid != 3001 ||
		unlinkedAttr.Mtime != 23 ||
		unlinkedAttr.Mtimensec != 24 {
		t.Errorf("active unlinked attrs = %#v", unlinkedAttr.Attr)
	}
	if errno := fixture.libraryRoot.Rmdir(ctx, "Movies"); errno != 0 {
		t.Fatalf("Rmdir errno = %v", errno)
	}

	if _, errno := fixture.root.Mkdir(ctx, "forbidden", 0o755, &gofuse.EntryOut{}); errno != syscall.EPERM {
		t.Errorf("GlobalRoot Mkdir errno = %v, want EPERM", errno)
	}
	if _, errno := fixture.libraryRoot.Mknod(ctx, "fifo", syscall.S_IFIFO|0o600, 0, &gofuse.EntryOut{}); errno != syscall.EOPNOTSUPP {
		t.Errorf("Mknod errno = %v, want EOPNOTSUPP", errno)
	}
	if _, errno := fixture.libraryRoot.Symlink(ctx, "target", "link", &gofuse.EntryOut{}); errno != syscall.EOPNOTSUPP {
		t.Errorf("Symlink errno = %v, want EOPNOTSUPP", errno)
	}
	if _, errno := fixture.libraryRoot.Link(ctx, fixture.libraryRoot, "hardlink", &gofuse.EntryOut{}); errno != syscall.EOPNOTSUPP {
		t.Errorf("Link errno = %v, want EOPNOTSUPP", errno)
	}
	if _, errno := fixture.libraryRoot.Readlink(ctx); errno != syscall.EOPNOTSUPP {
		t.Errorf("Readlink errno = %v, want EOPNOTSUPP", errno)
	}
}

func TestWritableFuseAppendCreateAndTruncatingOpen(t *testing.T) {
	fixture := newWritableFuseFixture(t)
	ctx := fuseCallerContext(1234, 5678)
	inode, file, _, errno := fixture.libraryRoot.Create(
		ctx,
		"notes.nfo",
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_APPEND,
		0o640,
		&gofuse.EntryOut{},
	)
	if errno != 0 {
		t.Fatalf("Create errno = %v", errno)
	}
	handle := file.(*overlayHandle)
	if written, errno := handle.Write(ctx, []byte("a"), 0); errno != 0 || written != 1 {
		t.Fatalf("first append Write = %d/%v", written, errno)
	}
	if written, errno := handle.Write(ctx, []byte("b"), 0); errno != 0 || written != 1 {
		t.Fatalf("second append Write = %d/%v", written, errno)
	}
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release created handle errno = %v", errno)
	}

	fileNode := inode.Operations().(*node)
	truncated, _, errno := fileNode.Open(ctx, unix.O_RDWR|unix.O_TRUNC)
	if errno != 0 {
		t.Fatalf("Open O_TRUNC errno = %v", errno)
	}
	truncatedHandle := truncated.(*overlayHandle)
	defer truncatedHandle.Release(context.Background())
	var attr gofuse.AttrOut
	if errno := fileNode.Getattr(ctx, truncatedHandle, &attr); errno != 0 {
		t.Fatalf("Getattr after O_TRUNC errno = %v", errno)
	}
	if attr.Size != 0 {
		t.Errorf("size after O_TRUNC = %d, want 0 before Open returns", attr.Size)
	}
}

func TestWritableFuseRenameAndFailureMatrix(t *testing.T) {
	fixture := newWritableFuseFixture(t)
	ctx := fuseCallerContext(1234, 5678)
	moviesInode, errno := fixture.libraryRoot.Mkdir(
		ctx,
		"Movies",
		0o750,
		&gofuse.EntryOut{},
	)
	if errno != 0 {
		t.Fatalf("Mkdir errno = %v", errno)
	}
	movies := moviesInode.Operations().(*node)

	create := func(name, content string) (*node, *overlayHandle) {
		t.Helper()
		inode, file, _, errno := movies.Create(
			ctx,
			name,
			unix.O_RDWR|unix.O_CREAT|unix.O_EXCL,
			0o640,
			&gofuse.EntryOut{},
		)
		if errno != 0 {
			t.Fatalf("Create(%q) errno = %v", name, errno)
		}
		handle := file.(*overlayHandle)
		t.Cleanup(func() {
			handle.Release(context.Background())
		})
		if written, errno := handle.Write(ctx, []byte(content), 0); errno != 0 ||
			written != uint32(len(content)) {
			t.Fatalf("Write(%q) = %d/%v", name, written, errno)
		}
		return inode.Operations().(*node), handle
	}

	sourceNode, sourceHandle := create("source", "source")
	targetNode, targetHandle := create("target", "target")
	if errno := movies.Rename(ctx, "source", movies, "target", 0); errno != 0 {
		t.Fatalf("Overlay replacement Rename errno = %v", errno)
	}
	if written, errno := targetHandle.Write(ctx, []byte("OLD"), 0); errno != 0 || written != 3 {
		t.Fatalf("Write replaced active handle = %d/%v, want 3/OK", written, errno)
	}
	var replacedAttr gofuse.AttrOut
	if errno := targetNode.Setattr(ctx, targetHandle, &gofuse.SetAttrIn{
		SetAttrInCommon: gofuse.SetAttrInCommon{
			Valid: gofuse.FATTR_MODE |
				gofuse.FATTR_UID |
				gofuse.FATTR_GID |
				gofuse.FATTR_SIZE |
				gofuse.FATTR_ATIME |
				gofuse.FATTR_MTIME,
			Mode:      0o602,
			Owner:     gofuse.Owner{Uid: 4000, Gid: 4001},
			Size:      4,
			Atime:     31,
			Atimensec: 32,
			Mtime:     33,
			Mtimensec: 34,
		},
	}, &replacedAttr); errno != 0 {
		t.Fatalf("Setattr replaced active handle errno = %v", errno)
	}
	if replacedAttr.Size != 4 ||
		replacedAttr.Mode != syscall.S_IFREG|0o602 ||
		replacedAttr.Uid != 4000 ||
		replacedAttr.Gid != 4001 ||
		replacedAttr.Atime != 31 ||
		replacedAttr.Atimensec != 32 ||
		replacedAttr.Mtime != 33 ||
		replacedAttr.Mtimensec != 34 {
		t.Errorf("replaced active setattr attrs = %#v", replacedAttr.Attr)
	}
	result, errno := targetHandle.Read(ctx, make([]byte, 4), 0)
	if errno != 0 {
		t.Fatalf("Read replaced active handle errno = %v", errno)
	}
	content, status := result.Bytes(make([]byte, 4))
	result.Done()
	if status != gofuse.OK || string(content) != "OLDg" {
		t.Errorf("replaced active handle = %q/%v", content, status)
	}
	if errno := targetNode.Getattr(ctx, targetHandle, &replacedAttr); errno != 0 {
		t.Fatalf("Getattr replaced active handle errno = %v", errno)
	}
	if replacedAttr.Size != 4 ||
		replacedAttr.Mode != syscall.S_IFREG|0o602 ||
		replacedAttr.Uid != 4000 ||
		replacedAttr.Gid != 4001 ||
		replacedAttr.Mtime != 33 ||
		replacedAttr.Mtimensec != 34 {
		t.Errorf("replaced active attrs = %#v", replacedAttr.Attr)
	}
	reopened, _, errno := sourceNode.Open(ctx, unix.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open replacement target errno = %v", errno)
	}
	replacementHandle := reopened.(*overlayHandle)
	defer replacementHandle.Release(context.Background())
	result, errno = replacementHandle.Read(ctx, make([]byte, 6), 0)
	if errno != 0 {
		t.Fatalf("Read replacement target errno = %v", errno)
	}
	content, status = result.Bytes(make([]byte, 6))
	result.Done()
	if status != gofuse.OK || string(content) != "source" {
		t.Errorf("replacement target content = %q/%v", content, status)
	}
	_ = sourceHandle

	create("no-replace-source", "a")
	create("no-replace-target", "b")
	if errno := movies.Rename(
		ctx,
		"no-replace-source",
		movies,
		"no-replace-target",
		unix.RENAME_NOREPLACE,
	); errno != syscall.EEXIST {
		t.Errorf("RENAME_NOREPLACE errno = %v, want EEXIST", errno)
	}
	if errno := movies.Rename(
		ctx,
		"no-replace-source",
		movies,
		"no-replace-target",
		unix.RENAME_EXCHANGE,
	); errno != syscall.EINVAL {
		t.Errorf("unsupported Rename flags errno = %v, want EINVAL", errno)
	}

	directoryInode, errno := movies.Mkdir(ctx, "directory", 0o750, &gofuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Mkdir directory errno = %v", errno)
	}
	if errno := movies.Rename(
		ctx,
		"no-replace-source",
		movies,
		"directory",
		0,
	); errno != syscall.EISDIR {
		t.Errorf("file-over-directory errno = %v, want EISDIR", errno)
	}
	create("file-target", "file")
	if errno := movies.Rename(
		ctx,
		"directory",
		movies,
		"file-target",
		0,
	); errno != syscall.ENOTDIR {
		t.Errorf("directory-over-file errno = %v, want ENOTDIR", errno)
	}
	_ = directoryInode

	if _, err := fixture.repository.Update(
		context.Background(),
		namespace.Operation{
			Name:  "projection.create",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{"jellyfin/Movies/movie.mkv"},
				NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
			},
		},
		func(transaction *namespace.Transaction) error {
			_, err := transaction.CreateNode(context.Background(), namespace.CreateNode{
				ParentID: movies.record.ID,
				Name:     "movie.mkv",
				Kind:     namespace.ProjectionFile,
				Source: &namespace.ProjectionSource{
					Path:          "/missing/movie.mkv",
					DeviceID:      1,
					Inode:         2,
					Size:          3,
					AtimeNS:       4,
					MtimeNS:       5,
					SourceCtimeNS: 6,
					VerifiedAtNS:  7,
				},
				Creator: namespace.Actor{Kind: namespace.ActorSystem},
				Attributes: namespace.Attributes{
					Mode: 0o444,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create Projection target: %v", err)
	}
	if errno := movies.Rename(
		ctx,
		"no-replace-source",
		movies,
		"movie.mkv",
		0,
	); errno != syscall.EEXIST {
		t.Errorf("Overlay-over-Projection errno = %v, want EEXIST", errno)
	}

	if inode, handle, _, errno := movies.Create(
		ctx,
		"file-target",
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL,
		0o640,
		&gofuse.EntryOut{},
	); errno != syscall.EEXIST || inode != nil || handle != nil {
		t.Errorf("exclusive Create existing = inode %v handle %v errno %v, want nil/nil/EEXIST", inode, handle, errno)
	}
}

func TestWritableFuseMissingObjectFailsClosed(t *testing.T) {
	fixture := newWritableFuseFixture(t)
	ctx := fuseCallerContext(1234, 5678)
	inode, file, _, errno := fixture.libraryRoot.Create(
		ctx,
		"poster.jpg",
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL,
		0o640,
		&gofuse.EntryOut{},
	)
	if errno != 0 {
		t.Fatalf("Create errno = %v", errno)
	}
	handle := file.(*overlayHandle)
	record := inode.Operations().(*node).record
	if errno := handle.Release(context.Background()); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
	if record.ObjectID == nil {
		t.Fatal("created Overlay has no object ID")
	}
	if err := fixture.store.DeleteObject(*record.ObjectID); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	fileNode := inode.Operations().(*node)
	if opened, _, errno := fileNode.Open(ctx, unix.O_RDONLY); errno != syscall.EIO || opened != nil {
		t.Errorf("Open missing object = %v/%v, want nil/EIO", opened, errno)
	}
	if errno := fileNode.Getattr(ctx, nil, &gofuse.AttrOut{}); errno != syscall.EIO {
		t.Errorf("Getattr missing object errno = %v, want EIO", errno)
	}
}

type writableFuseFixture struct {
	repository  *namespace.Repository
	store       *objectstore.Store
	library     namespace.Library
	root        *node
	libraryRoot *node
}

func newWritableFuseFixture(t *testing.T) writableFuseFixture {
	t.Helper()
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
			t.Errorf("database Close: %v", err)
		}
	})
	store, err := objectstore.Open(filepath.Join(root, "objects"), opened)
	if err != nil {
		t.Fatalf("objectstore Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("objectstore Close: %v", err)
		}
	})
	repository := namespace.NewRepository(opened)
	var library namespace.Library
	if _, err := repository.Update(
		context.Background(),
		namespace.Operation{
			Name:  "library.create",
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
	rootNode, err := newRoot(context.Background(), repository, store)
	if err != nil {
		t.Fatalf("newRoot: %v", err)
	}
	initializeNodeFS(rootNode)
	libraryInode, errno := rootNode.Lookup(context.Background(), library.Name, &gofuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Lookup Library errno = %v", errno)
	}
	return writableFuseFixture{
		repository:  repository,
		store:       store,
		library:     library,
		root:        rootNode,
		libraryRoot: libraryInode.Operations().(*node),
	}
}

func fuseCallerContext(uid, gid uint32) context.Context {
	return gofuse.NewContext(context.Background(), &gofuse.Caller{
		Owner: gofuse.Owner{Uid: uid, Gid: gid},
		Pid:   42,
	})
}

func openTestObjectFile(t *testing.T, flags int) *objectstore.File {
	t.Helper()
	return openObjectFilePath(t, filepath.Join(t.TempDir(), "object"), flags|unix.O_CREAT)
}

func openObjectFilePath(t *testing.T, path string, flags int) *objectstore.File {
	t.Helper()
	raw, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	return &objectstore.File{File: raw, ObjectID: 1}
}
