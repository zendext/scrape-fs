package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
	"github.com/zendext/scrape-fs/internal/sourcefile"
	"golang.org/x/sys/unix"
)

func TestProjectionFileOpenReadGetattrAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Dune.mkv")
	content := []byte("0123456789")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	record := projectionNodeForSource(t, 4, 3, "Dune.mkv", path)
	reader := &fakeNamespaceReader{
		nodes: map[int64]namespace.Node{record.ID: record},
	}
	current := &node{reader: reader, record: record}

	handle, fuseFlags, errno := current.Open(context.Background(), unix.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	if fuseFlags&gofuse.FOPEN_DIRECT_IO == 0 {
		t.Errorf("Open flags = %#x, want FOPEN_DIRECT_IO", fuseFlags)
	}
	readerHandle, ok := handle.(gofs.FileReader)
	if !ok {
		t.Fatalf("handle %T does not implement FileReader", handle)
	}
	for _, read := range []struct {
		offset int64
		size   int
		want   string
	}{
		{offset: 0, size: 4, want: "0123"},
		{offset: 6, size: 8, want: "6789"},
		{offset: int64(len(content)), size: 4, want: ""},
	} {
		result, errno := readerHandle.Read(
			context.Background(),
			make([]byte, read.size),
			read.offset,
		)
		if errno != 0 {
			t.Fatalf("Read offset %d errno = %v", read.offset, errno)
		}
		got, status := result.Bytes(make([]byte, read.size))
		result.Done()
		if status != gofuse.OK || string(got) != read.want {
			t.Errorf(
				"Read offset %d = %q/%v, want %q/OK",
				read.offset,
				got,
				status,
				read.want,
			)
		}
	}

	getattr, ok := handle.(gofs.FileGetattrer)
	if !ok {
		t.Fatalf("handle %T does not implement FileGetattrer", handle)
	}
	var attr gofuse.AttrOut
	if errno := getattr.Getattr(context.Background(), &attr); errno != 0 {
		t.Fatalf("Getattr errno = %v", errno)
	}
	if attr.Ino != uint64(record.ID) ||
		attr.Mode != syscall.S_IFREG|uint32(record.Attributes.Mode) ||
		attr.Size != uint64(len(content)) ||
		attr.Nlink != 1 {
		t.Errorf("Getattr attrs = %#v", attr.Attr)
	}

	writer, ok := handle.(gofs.FileWriter)
	if !ok {
		t.Fatalf("handle %T does not implement FileWriter", handle)
	}
	if written, errno := writer.Write(
		context.Background(),
		[]byte("changed"),
		0,
	); written != 0 || errno != syscall.EROFS {
		t.Errorf("Write = %d/%v, want 0/EROFS", written, errno)
	}
	allocater, ok := handle.(gofs.FileAllocater)
	if !ok {
		t.Fatalf("handle %T does not implement FileAllocater", handle)
	}
	if errno := allocater.Allocate(context.Background(), 0, 1, 0); errno != syscall.EOPNOTSUPP {
		t.Errorf("Allocate errno = %v, want EOPNOTSUPP", errno)
	}

	releaser, ok := handle.(gofs.FileReleaser)
	if !ok {
		t.Fatalf("handle %T does not implement FileReleaser", handle)
	}
	if errno := releaser.Release(context.Background()); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
	if errno := releaser.Release(context.Background()); errno != 0 {
		t.Errorf("second Release errno = %v", errno)
	}
	if result, errno := readerHandle.Read(
		context.Background(),
		make([]byte, 1),
		0,
	); result != nil || errno != syscall.EBADF {
		t.Errorf("Read after Release = %v/%v, want nil/EBADF", result, errno)
	}
}

func TestProjectionFileOpenRejectsWritesAndChangedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile original: %v", err)
	}
	record := projectionNodeForSource(t, 4, 3, "source.mkv", path)
	reader := &fakeNamespaceReader{
		nodes: map[int64]namespace.Node{record.ID: record},
	}
	current := &node{reader: reader, record: record}

	for name, flags := range map[string]uint32{
		"O_WRONLY": unix.O_WRONLY,
		"O_RDWR":   unix.O_RDWR,
		"O_APPEND": unix.O_RDONLY | unix.O_APPEND,
		"O_TRUNC":  unix.O_RDONLY | unix.O_TRUNC,
	} {
		t.Run(name, func(t *testing.T) {
			handle, _, errno := current.Open(context.Background(), flags)
			if handle != nil || errno != syscall.EROFS {
				t.Errorf("Open = %v/%v, want nil/EROFS", handle, errno)
			}
		})
	}
	for name, valid := range map[string]uint32{
		"truncate": gofuse.FATTR_SIZE,
		"chmod":    gofuse.FATTR_MODE,
		"chown":    gofuse.FATTR_UID | gofuse.FATTR_GID,
		"utimens":  gofuse.FATTR_ATIME | gofuse.FATTR_MTIME,
	} {
		t.Run(name, func(t *testing.T) {
			if errno := current.Setattr(
				context.Background(),
				nil,
				&gofuse.SetAttrIn{
					SetAttrInCommon: gofuse.SetAttrInCommon{Valid: valid},
				},
				&gofuse.AttrOut{},
			); errno != syscall.EROFS {
				t.Errorf("Setattr errno = %v, want EROFS", errno)
			}
		})
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove source: %v", err)
	}
	if handle, _, errno := current.Open(
		context.Background(),
		unix.O_RDONLY,
	); handle != nil || errno != syscall.ENOENT {
		t.Errorf("Open missing source = %v/%v, want nil/ENOENT", handle, errno)
	}
	var missingAttr gofuse.AttrOut
	if errno := current.Getattr(
		context.Background(),
		nil,
		&missingAttr,
	); errno != 0 {
		t.Fatalf("Getattr missing source errno = %v", errno)
	}
	if missingAttr.Size != uint64(record.Source.Size) ||
		missingAttr.Mtime != uint64(record.Source.MtimeNS/int64(1_000_000_000)) {
		t.Errorf("missing source attrs = %#v, want cached source attrs", missingAttr.Attr)
	}

	replacement := filepath.Join(filepath.Dir(path), "replacement.mkv")
	if err := os.WriteFile(replacement, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile replacement: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("Rename replacement: %v", err)
	}
	if handle, _, errno := current.Open(
		context.Background(),
		unix.O_RDONLY,
	); handle != nil || errno != syscall.ESTALE {
		t.Errorf("Open stale source = %v/%v, want nil/ESTALE", handle, errno)
	}
	var staleAttr gofuse.AttrOut
	if errno := current.Getattr(
		context.Background(),
		nil,
		&staleAttr,
	); errno != 0 {
		t.Fatalf("Getattr stale source errno = %v", errno)
	}
	if staleAttr.Size != uint64(record.Source.Size) {
		t.Errorf("stale source size = %d, want cached %d", staleAttr.Size, record.Source.Size)
	}
}

func TestProjectionFuseRenameUnlinkAndHandleLifetime(t *testing.T) {
	fixture := newWritableFuseFixture(t)
	ctx := fuseCallerContext(1234, 5678)
	service := projection.NewService(fixture.repository)
	sourcePath := filepath.Join(t.TempDir(), "source.mkv")
	sourceContent := []byte("projection content")
	if err := os.WriteFile(sourcePath, sourceContent, 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	created, err := service.Create(context.Background(), projection.CreateRequest{
		SourcePath: sourcePath,
		Target:     "jellyfin:/Movies/source.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	})
	if err != nil {
		t.Fatalf("Create Projection: %v", err)
	}

	movies := lookupFuseDirectory(t, fixture.libraryRoot, "Movies")
	fileNode := lookupFuseFile(t, movies, created.Name)
	fileHandle, _, errno := fileNode.Open(ctx, unix.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open Projection errno = %v", errno)
	}
	handle := fileHandle.(gofs.FileReader)
	t.Cleanup(func() {
		fileHandle.(gofs.FileReleaser).Release(context.Background())
	})

	secondSource := filepath.Join(t.TempDir(), "second.mkv")
	if err := os.WriteFile(secondSource, []byte("second"), 0o600); err != nil {
		t.Fatalf("WriteFile second source: %v", err)
	}
	if _, err := service.Create(context.Background(), projection.CreateRequest{
		SourcePath: secondSource,
		Target:     "jellyfin:/Movies/existing.mkv",
		Actor:      namespace.Actor{Kind: namespace.ActorSystem},
	}); err != nil {
		t.Fatalf("Create target Projection: %v", err)
	}
	for name, flags := range map[string]uint32{
		"flags zero":       0,
		"RENAME_NOREPLACE": unix.RENAME_NOREPLACE,
	} {
		t.Run(name, func(t *testing.T) {
			if errno := movies.Rename(
				ctx,
				"source.mkv",
				movies,
				"existing.mkv",
				flags,
			); errno != syscall.EEXIST {
				t.Errorf("Rename existing target errno = %v, want EEXIST", errno)
			}
		})
	}

	other := createFuseTestLibrary(t, fixture.repository, "plex", "plex")
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
	otherRoot := lookupFuseDirectory(t, fixture.root, other.Name)
	otherMovies := lookupFuseDirectory(t, otherRoot, "Movies")
	if errno := movies.Rename(
		ctx,
		"source.mkv",
		otherMovies,
		"moved.mkv",
		0,
	); errno != syscall.EXDEV {
		t.Errorf("cross-Library Rename errno = %v, want EXDEV", errno)
	}

	if errno := movies.Rename(
		ctx,
		"source.mkv",
		movies,
		"renamed.mkv",
		0,
	); errno != 0 {
		t.Fatalf("Rename Projection errno = %v", errno)
	}
	if _, err := fixture.repository.Child(
		context.Background(),
		movies.record.ID,
		"source.mkv",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("old path error = %v, want ErrNotFound", err)
	}
	renamed, err := fixture.repository.Child(
		context.Background(),
		movies.record.ID,
		"renamed.mkv",
	)
	if err != nil {
		t.Fatalf("read renamed Projection: %v", err)
	}
	if renamed.ID != created.ID {
		t.Errorf("renamed ID = %d, want %d", renamed.ID, created.ID)
	}
	assertProjectionHandleContent(t, handle, sourceContent)

	if errno := movies.Unlink(ctx, "renamed.mkv"); errno != 0 {
		t.Fatalf("Unlink Projection errno = %v", errno)
	}
	if _, err := fixture.repository.Child(
		context.Background(),
		movies.record.ID,
		"renamed.mkv",
	); !errors.Is(err, namespace.ErrNotFound) {
		t.Errorf("unlinked path error = %v, want ErrNotFound", err)
	}
	var unlinkedAttr gofuse.AttrOut
	if errno := fileNode.Getattr(
		ctx,
		fileHandle,
		&unlinkedAttr,
	); errno != 0 {
		t.Errorf("Getattr active unlinked handle errno = %v", errno)
	}
	if unlinkedAttr.Size != uint64(len(sourceContent)) {
		t.Errorf(
			"active unlinked handle size = %d, want %d",
			unlinkedAttr.Size,
			len(sourceContent),
		)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != string(sourceContent) {
		t.Errorf("source after namespace mutations = %q/%v", got, err)
	}
	assertProjectionHandleContent(t, handle, sourceContent)
}

func TestProjectionFuseLifecycleRefreshesRenamedParentPath(t *testing.T) {
	fixture := newWritableFuseFixture(t)
	ctx := fuseCallerContext(1234, 5678)
	sourcePath := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	if _, err := projection.NewService(fixture.repository).Create(
		context.Background(),
		projection.CreateRequest{
			SourcePath: sourcePath,
			Target:     "jellyfin:/Movies/Staging/source.mkv",
			Actor:      namespace.Actor{Kind: namespace.ActorSystem},
		},
	); err != nil {
		t.Fatalf("Create Projection: %v", err)
	}

	movies := lookupFuseDirectory(t, fixture.libraryRoot, "Movies")
	staging := lookupFuseDirectory(t, movies, "Staging")
	if errno := movies.Rename(
		ctx,
		"Staging",
		movies,
		"Renamed",
		0,
	); errno != 0 {
		t.Fatalf("Rename parent Directory errno = %v", errno)
	}
	if errno := staging.Rename(
		ctx,
		"source.mkv",
		staging,
		"moved.mkv",
		0,
	); errno != 0 {
		t.Fatalf("Rename Projection through stale parent node errno = %v", errno)
	}
	if _, err := fixture.repository.Lookup(
		context.Background(),
		fixture.library.RootNodeID,
		"Movies/Renamed/moved.mkv",
	); err != nil {
		t.Fatalf("Lookup moved Projection: %v", err)
	}
	if errno := staging.Unlink(ctx, "moved.mkv"); errno != 0 {
		t.Fatalf("Unlink Projection through stale parent node errno = %v", errno)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Errorf("source after lifecycle mutations: %v", err)
	}
}

func TestProjectionFuseRenamesBatchOwnedProjection(t *testing.T) {
	fixture := newWritableFuseFixture(t)
	ctx := fuseCallerContext(1234, 5678)
	sourcePath := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(sourcePath, []byte("batch source"), 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	data, err := json.Marshal(projectionbatch.Document{
		Schema:  projectionbatch.BatchSchema,
		Library: "jellyfin",
		Entries: []projectionbatch.Entry{
			{Source: sourcePath, Target: "Movies/source.mkv"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal Batch: %v", err)
	}
	batches := projectionbatch.NewService(fixture.repository)
	validation, err := batches.Validate(context.Background(), data)
	if err != nil {
		t.Fatalf("Validate Batch: %v", err)
	}
	applied, err := batches.Apply(context.Background(), projectionbatch.ApplyRequest{
		Data:           data,
		ExpectedDigest: validation.Digest,
		Actor:          namespace.Actor{Kind: namespace.ActorSystem},
		Caller:         projectionbatch.Caller{UID: 1234, GID: 5678},
	})
	if err != nil {
		t.Fatalf("Apply Batch: %v", err)
	}

	movies := lookupFuseDirectory(t, fixture.libraryRoot, "Movies")
	if errno := movies.Rename(
		ctx,
		"source.mkv",
		movies,
		"renamed.mkv",
		0,
	); errno != 0 {
		t.Fatalf("Rename Batch Projection errno = %v", errno)
	}
	moved, err := fixture.repository.Lookup(
		context.Background(),
		fixture.library.RootNodeID,
		"Movies/renamed.mkv",
	)
	if err != nil {
		t.Fatalf("Lookup moved Batch Projection: %v", err)
	}
	if moved.OriginScrapeID == nil {
		t.Fatal("moved Batch Projection OriginScrapeID = nil")
	}
	detail, err := batches.Show(context.Background(), applied.BatchID)
	if err != nil {
		t.Fatalf("Show Batch after FUSE rename: %v", err)
	}
	if len(detail.Operations) != 2 ||
		detail.Operations[1].Name != "projection.move" {
		t.Errorf("Batch operations after FUSE rename = %#v", detail.Operations)
	}
}

func TestProjectionFileReopensAfterDatabaseRestart(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "scrapefs.db")
	options := database.Options{LockPath: filepath.Join(root, "run", "scrapefsd.lock")}
	sourcePath := filepath.Join(root, "source.mkv")
	content := []byte("persistent projection")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	first, err := database.OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	firstRepository := namespace.NewRepository(first)
	library := createFuseTestLibrary(t, firstRepository, "jellyfin", "jellyfin")
	created, err := projection.NewService(firstRepository).Create(
		context.Background(),
		projection.CreateRequest{
			SourcePath: sourcePath,
			Target:     "jellyfin:/Movies/source.mkv",
			Actor:      namespace.Actor{Kind: namespace.ActorSystem},
		},
	)
	if err != nil {
		first.Close()
		t.Fatalf("Create Projection: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := database.OpenWithOptions(context.Background(), databasePath, options)
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	})
	rootNode, err := newRoot(
		context.Background(),
		namespace.NewRepository(second),
		&fakeObjectMetadataReader{},
	)
	if err != nil {
		t.Fatalf("newRoot after restart: %v", err)
	}
	initializeNodeFS(rootNode)
	libraryRoot := lookupFuseDirectory(t, rootNode, library.Name)
	movies := lookupFuseDirectory(t, libraryRoot, "Movies")
	reopened := lookupFuseFile(t, movies, created.Name)
	if reopened.record.ID != created.ID {
		t.Errorf("reopened ID = %d, want %d", reopened.record.ID, created.ID)
	}
	fileHandle, _, errno := reopened.Open(context.Background(), unix.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open after restart errno = %v", errno)
	}
	defer fileHandle.(gofs.FileReleaser).Release(context.Background())
	assertProjectionHandleContent(t, fileHandle.(gofs.FileReader), content)
}

func TestProjectionFuseMapsBatchOwnedMutationToReadOnly(t *testing.T) {
	if errno := errorNumber(namespace.ErrBatchOwnedProjection); errno != syscall.EROFS {
		t.Errorf("Batch-owned Projection errno = %v, want EROFS", errno)
	}
}

func TestErrorNumberFailsClosedAndPreservesSourceErrnos(t *testing.T) {
	if errno := errorNumber(errors.New("opaque database failure")); errno != syscall.EIO {
		t.Errorf("opaque error errno = %v, want EIO", errno)
	}
	for _, want := range []syscall.Errno{
		syscall.ENOENT,
		syscall.ENOTDIR,
		syscall.ENODEV,
		syscall.ESTALE,
	} {
		err := &os.PathError{Op: "open", Path: "/source", Err: want}
		if errno := errorNumber(err); errno != want {
			t.Errorf("wrapped %v errno = %v, want %v", want, errno, want)
		}
	}
}

func projectionNodeForSource(
	t *testing.T,
	id int64,
	parentID int64,
	name string,
	path string,
) namespace.Node {
	t.Helper()
	opened, err := sourcefile.Open(path)
	if err != nil {
		t.Fatalf("sourcefile.Open: %v", err)
	}
	metadata := opened.Metadata
	if err := opened.Close(); err != nil {
		t.Fatalf("Close source metadata handle: %v", err)
	}
	record := testNode(id, parentID, name, namespace.ProjectionFile)
	record.Attributes.Mode = 0o444
	record.Source = &namespace.ProjectionSource{
		Path:          metadata.Path,
		DeviceID:      metadata.DeviceID,
		Inode:         metadata.Inode,
		Size:          metadata.Size,
		AtimeNS:       metadata.AtimeNS,
		MtimeNS:       metadata.MtimeNS,
		SourceCtimeNS: metadata.CtimeNS,
		VerifiedAtNS:  1,
	}
	return record
}

func lookupFuseDirectory(t *testing.T, parent *node, name string) *node {
	t.Helper()
	inode, errno := parent.Lookup(context.Background(), name, &gofuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Lookup directory %q errno = %v", name, errno)
	}
	current := inode.Operations().(*node)
	if !directoryKind(current.record.Kind) {
		t.Fatalf("Lookup %q kind = %s, want directory", name, current.record.Kind)
	}
	return current
}

func lookupFuseFile(t *testing.T, parent *node, name string) *node {
	t.Helper()
	inode, errno := parent.Lookup(context.Background(), name, &gofuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Lookup file %q errno = %v", name, errno)
	}
	current := inode.Operations().(*node)
	if directoryKind(current.record.Kind) {
		t.Fatalf("Lookup %q kind = %s, want file", name, current.record.Kind)
	}
	return current
}

func createFuseTestLibrary(
	t *testing.T,
	repository *namespace.Repository,
	name string,
	profile string,
) namespace.Library {
	t.Helper()
	var library namespace.Library
	if _, err := repository.Update(
		context.Background(),
		namespace.Operation{
			Name:  "fuse.test.library.create",
			Actor: namespace.Actor{Kind: namespace.ActorSystem},
			Details: namespace.OperationDetails{
				Paths:     []string{name},
				NodeKinds: []namespace.NodeKind{namespace.LibraryRoot},
			},
		},
		func(transaction *namespace.Transaction) error {
			var err error
			library, err = transaction.CreateLibrary(
				context.Background(),
				namespace.CreateLibrary{
					Name:           name,
					ProfileName:    profile,
					ProfileVersion: 1,
					Creator:        namespace.Actor{Kind: namespace.ActorSystem},
					UID:            uint32(os.Geteuid()),
					GID:            uint32(os.Getegid()),
					RootMode:       0o775,
					DirectoryMode:  0o775,
					ProjectionMode: 0o444,
					OverlayMode:    0o664,
				},
			)
			return err
		},
	); err != nil {
		t.Fatalf("CreateLibrary(%q): %v", name, err)
	}
	return library
}

func assertProjectionHandleContent(
	t *testing.T,
	handle gofs.FileReader,
	want []byte,
) {
	t.Helper()
	result, errno := handle.Read(context.Background(), make([]byte, len(want)), 0)
	if errno != 0 {
		t.Fatalf("Read active Projection handle errno = %v", errno)
	}
	got, status := result.Bytes(make([]byte, len(want)))
	result.Done()
	if status != gofuse.OK || string(got) != string(want) {
		t.Errorf("active Projection handle = %q/%v, want %q/OK", got, status, want)
	}
}
