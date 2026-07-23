package fuse

import (
	"context"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/namespace"
)

func TestRootLookupAndReaddirExposeOnlyActiveLibraries(t *testing.T) {
	reader := &fakeNamespaceReader{
		global: testNode(1, 0, "", namespace.GlobalRoot),
		libraries: []namespace.Library{
			{Name: "jellyfin", RootNodeID: 2},
			{Name: "plex", RootNodeID: 3},
		},
		nodes: map[int64]namespace.Node{
			2: testNode(2, 1, "jellyfin", namespace.LibraryRoot),
			3: testNode(3, 1, "plex", namespace.LibraryRoot),
		},
	}
	root, err := newRoot(context.Background(), reader)
	if err != nil {
		t.Fatalf("newRoot: %v", err)
	}
	initializeNodeFS(root)

	var entry gofuse.EntryOut
	child, errno := root.Lookup(context.Background(), "jellyfin", &entry)
	if errno != 0 {
		t.Fatalf("Lookup errno = %v", errno)
	}
	if got := child.StableAttr().Ino; got != 2 {
		t.Errorf("lookup inode = %d, want 2", got)
	}
	if entry.Ino != 2 || entry.Mode != syscall.S_IFDIR|0o775 {
		t.Errorf("lookup attrs = %#v", entry.Attr)
	}

	stream, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}
	entries := readDirEntries(t, stream)
	if len(entries) != 2 ||
		entries[0].Name != "jellyfin" || entries[0].Ino != 2 ||
		entries[1].Name != "plex" || entries[1].Ino != 3 {
		t.Errorf("entries = %#v", entries)
	}

	if child, errno := root.Lookup(context.Background(), "retired", &gofuse.EntryOut{}); child != nil ||
		errno != syscall.ENOENT {
		t.Errorf("Lookup retired = child %#v errno %v, want nil/ENOENT", child, errno)
	}
}

func TestDirectoryLookupGetattrAndReaddirUsePersistedNodes(t *testing.T) {
	directory := testNode(10, 2, "Movies", namespace.Directory)
	projection := testNode(11, 10, "Dune.mkv", namespace.ProjectionFile)
	projection.Attributes = namespace.Attributes{Mode: 0o440, UID: 1234, GID: 5678}
	projection.Source = &namespace.ProjectionSource{
		Path:          "/srv/media/Dune.mkv",
		Size:          42,
		AtimeNS:       1_000_000_002,
		MtimeNS:       2_000_000_003,
		SourceCtimeNS: 3_000_000_004,
	}
	projection.VirtualCtimeNS = 4_000_000_005
	overlay := testNode(12, 10, "poster.jpg", namespace.OverlayFile)
	objectID := int64(9)
	overlay.ObjectID = &objectID

	reader := &fakeNamespaceReader{
		nodes: map[int64]namespace.Node{
			directory.ID:  directory,
			projection.ID: projection,
			overlay.ID:    overlay,
		},
		children: map[int64][]namespace.Node{
			directory.ID: {projection, overlay},
		},
		child: map[string]namespace.Node{
			fakeChildKey(directory.ID, projection.Name): projection,
			fakeChildKey(directory.ID, overlay.Name):    overlay,
		},
	}
	current := &node{reader: reader, record: directory}
	initializeNodeFS(current)

	var directoryAttr gofuse.AttrOut
	if errno := current.Getattr(context.Background(), nil, &directoryAttr); errno != 0 {
		t.Fatalf("directory Getattr errno = %v", errno)
	}
	if directoryAttr.Ino != 10 || directoryAttr.Mode != syscall.S_IFDIR|0o775 {
		t.Errorf("directory attrs = %#v", directoryAttr.Attr)
	}

	var entry gofuse.EntryOut
	child, errno := current.Lookup(context.Background(), projection.Name, &entry)
	if errno != 0 {
		t.Fatalf("Lookup errno = %v", errno)
	}
	if child.StableAttr().Ino != uint64(projection.ID) {
		t.Errorf("stable inode = %d, want %d", child.StableAttr().Ino, projection.ID)
	}
	if entry.Ino != uint64(projection.ID) ||
		entry.Mode != syscall.S_IFREG|0o440 ||
		entry.Size != 42 ||
		entry.Uid != 1234 ||
		entry.Gid != 5678 ||
		entry.Atime != 1 || entry.Atimensec != 2 ||
		entry.Mtime != 2 || entry.Mtimensec != 3 ||
		entry.Ctime != 4 || entry.Ctimensec != 5 {
		t.Errorf("projection attrs = %#v", entry.Attr)
	}

	operations := child.Operations().(*node)
	var attr gofuse.AttrOut
	if errno := operations.Getattr(context.Background(), nil, &attr); errno != 0 {
		t.Fatalf("Getattr errno = %v", errno)
	}
	if attr.Ino != entry.Ino || attr.Size != entry.Size || attr.Mode != entry.Mode {
		t.Errorf("Getattr attrs = %#v, lookup attrs = %#v", attr.Attr, entry.Attr)
	}

	var overlayEntry gofuse.EntryOut
	overlayChild, errno := current.Lookup(context.Background(), overlay.Name, &overlayEntry)
	if errno != 0 {
		t.Fatalf("Overlay Lookup errno = %v", errno)
	}
	if overlayChild.StableAttr().Ino != uint64(overlay.ID) ||
		overlayEntry.Ino != uint64(overlay.ID) ||
		overlayEntry.Mode != syscall.S_IFREG|0o775 ||
		overlayEntry.Size != 0 {
		t.Errorf("overlay attrs = %#v", overlayEntry.Attr)
	}

	stream, errno := current.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}
	entries := readDirEntries(t, stream)
	if len(entries) != 2 ||
		entries[0].Name != projection.Name ||
		entries[0].Ino != 11 ||
		entries[0].Mode != syscall.S_IFREG ||
		entries[1].Name != overlay.Name ||
		entries[1].Ino != 12 ||
		entries[1].Mode != syscall.S_IFREG {
		t.Errorf("entries = %#v", entries)
	}
}

func TestGetattrRefreshesNodeByStableID(t *testing.T) {
	directory := testNode(10, 2, "Movies", namespace.Directory)
	projection := testNode(11, 10, "Dune.mkv", namespace.ProjectionFile)
	projection.Attributes = namespace.Attributes{Mode: 0o440, UID: 1000, GID: 1001}
	projection.Source = &namespace.ProjectionSource{
		Path:          "/srv/media/Dune.mkv",
		Size:          42,
		AtimeNS:       1,
		MtimeNS:       2,
		SourceCtimeNS: 3,
	}
	reader := &fakeNamespaceReader{
		nodes: map[int64]namespace.Node{
			projection.ID: projection,
		},
		child: map[string]namespace.Node{
			fakeChildKey(directory.ID, projection.Name): projection,
		},
	}
	current := &node{reader: reader, record: directory}
	initializeNodeFS(current)

	child, errno := current.Lookup(
		context.Background(),
		projection.Name,
		&gofuse.EntryOut{},
	)
	if errno != 0 {
		t.Fatalf("Lookup errno = %v", errno)
	}

	updated := projection
	updated.Attributes = namespace.Attributes{Mode: 0o400, UID: 2000, GID: 2001}
	updated.Source = &namespace.ProjectionSource{
		Path:          projection.Source.Path,
		Size:          84,
		AtimeNS:       4_000_000_005,
		MtimeNS:       6_000_000_007,
		SourceCtimeNS: 8_000_000_009,
	}
	updated.VirtualCtimeNS = 10_000_000_011
	reader.nodes[projection.ID] = updated

	var attr gofuse.AttrOut
	operations := child.Operations().(*node)
	if errno := operations.Getattr(context.Background(), nil, &attr); errno != 0 {
		t.Fatalf("Getattr errno = %v", errno)
	}
	if attr.Ino != uint64(updated.ID) ||
		attr.Mode != syscall.S_IFREG|0o400 ||
		attr.Uid != 2000 ||
		attr.Gid != 2001 ||
		attr.Size != 84 ||
		attr.Atime != 4 || attr.Atimensec != 5 ||
		attr.Mtime != 6 || attr.Mtimensec != 7 ||
		attr.Ctime != 10 || attr.Ctimensec != 11 {
		t.Errorf("refreshed attrs = %#v", attr.Attr)
	}
}

func TestFilesystemMapsReadErrorsAndRejectsMutation(t *testing.T) {
	global := &node{
		reader: &fakeNamespaceReader{
			global:       testNode(1, 0, "", namespace.GlobalRoot),
			libraryError: namespace.ErrLibraryNotFound,
		},
		record: testNode(1, 0, "", namespace.GlobalRoot),
	}
	initializeNodeFS(global)

	if child, errno := global.Lookup(context.Background(), "missing", &gofuse.EntryOut{}); child != nil ||
		errno != syscall.ENOENT {
		t.Errorf("Lookup missing = child %#v errno %v, want nil/ENOENT", child, errno)
	}
	if _, errno := global.Mkdir(
		context.Background(),
		"new",
		0o755,
		&gofuse.EntryOut{},
	); errno != syscall.EPERM {
		t.Errorf("GlobalRoot Mkdir errno = %v, want EPERM", errno)
	}
	if errno := global.Rmdir(context.Background(), "library"); errno != syscall.EPERM {
		t.Errorf("GlobalRoot Rmdir errno = %v, want EPERM", errno)
	}
	if errno := global.Unlink(context.Background(), "library"); errno != syscall.EPERM {
		t.Errorf("GlobalRoot Unlink errno = %v, want EPERM", errno)
	}
	if errno := global.Rename(
		context.Background(),
		"library",
		global,
		"renamed",
		0,
	); errno != syscall.EPERM {
		t.Errorf("GlobalRoot Rename errno = %v, want EPERM", errno)
	}
	if errno := global.Setattr(
		context.Background(),
		nil,
		&gofuse.SetAttrIn{},
		&gofuse.AttrOut{},
	); errno != syscall.EPERM {
		t.Errorf("GlobalRoot Setattr errno = %v, want EPERM", errno)
	}

	directory := &node{record: testNode(2, 1, "library", namespace.LibraryRoot)}
	if _, errno := directory.Mkdir(
		context.Background(),
		"new",
		0o755,
		&gofuse.EntryOut{},
	); errno != syscall.EPERM {
		t.Errorf("LibraryRoot Mkdir errno = %v, want EPERM", errno)
	}
	ordinary := &node{record: testNode(3, 2, "Movies", namespace.Directory)}
	if errno := ordinary.Unlink(context.Background(), "file"); errno != syscall.EROFS {
		t.Errorf("Directory Unlink errno = %v, want EROFS", errno)
	}
}

func TestMountOptionsApplyDocumentedFlagsAndTimeouts(t *testing.T) {
	configuration := config.FUSE{
		Mountpoint:      "/mnt/scrapefs",
		EntryTimeout:    2 * time.Second,
		AttrTimeout:     3 * time.Second,
		NegativeTimeout: 500 * time.Millisecond,
	}
	options := filesystemOptions(configuration)

	if !options.AllowOther {
		t.Fatal("AllowOther = false, want true")
	}
	wantOptions := []string{"default_permissions", "nodev", "nosuid", "noexec"}
	if len(options.Options) != len(wantOptions) {
		t.Fatalf("mount options = %v, want %v", options.Options, wantOptions)
	}
	for index := range wantOptions {
		if options.Options[index] != wantOptions[index] {
			t.Errorf("mount options = %v, want %v", options.Options, wantOptions)
			break
		}
	}
	if options.EntryTimeout == nil || *options.EntryTimeout != configuration.EntryTimeout ||
		options.AttrTimeout == nil || *options.AttrTimeout != configuration.AttrTimeout ||
		options.NegativeTimeout == nil || *options.NegativeTimeout != configuration.NegativeTimeout {
		t.Errorf("timeouts = entry %v attr %v negative %v", options.EntryTimeout, options.AttrTimeout, options.NegativeTimeout)
	}
	if options.RootStableAttr == nil || options.RootStableAttr.Ino != 1 {
		t.Errorf("root stable attr = %#v, want inode 1", options.RootStableAttr)
	}
	if !options.NullPermissions {
		t.Fatal("NullPermissions = false, want exact persisted permission bits")
	}
	if options.ExplicitDataCacheControl {
		t.Fatal("ExplicitDataCacheControl = true, want auto cache invalidation")
	}
}

func initializeNodeFS(root *node) {
	gofs.NewNodeFS(root, &gofs.Options{
		RootStableAttr: &gofs.StableAttr{Ino: uint64(root.record.ID)},
	})
}

func readDirEntries(t *testing.T, stream gofs.DirStream) []gofuse.DirEntry {
	t.Helper()
	defer stream.Close()

	var entries []gofuse.DirEntry
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("DirStream.Next errno = %v", errno)
		}
		entries = append(entries, entry)
	}
	return entries
}

func testNode(id, parentID int64, name string, kind namespace.NodeKind) namespace.Node {
	var parent *int64
	if id != 1 {
		parent = &parentID
	}
	return namespace.Node{
		ID:       id,
		ParentID: parent,
		Name:     name,
		Kind:     kind,
		Attributes: namespace.Attributes{
			Mode: fs.FileMode(0o775),
			UID:  1000,
			GID:  1001,
		},
		AtimeNS:        1,
		MtimeNS:        2,
		VirtualCtimeNS: 3,
	}
}

type fakeNamespaceReader struct {
	global       namespace.Node
	libraries    []namespace.Library
	nodes        map[int64]namespace.Node
	child        map[string]namespace.Node
	children     map[int64][]namespace.Node
	libraryError error
}

func (reader *fakeNamespaceReader) GlobalRoot(context.Context) (namespace.Node, error) {
	return reader.global, nil
}

func (reader *fakeNamespaceReader) Library(_ context.Context, name string) (namespace.Library, error) {
	if reader.libraryError != nil {
		return namespace.Library{}, reader.libraryError
	}
	for _, library := range reader.libraries {
		if library.Name == name {
			return library, nil
		}
	}
	return namespace.Library{}, namespace.ErrLibraryNotFound
}

func (reader *fakeNamespaceReader) Libraries(context.Context) ([]namespace.Library, error) {
	return append([]namespace.Library(nil), reader.libraries...), nil
}

func (reader *fakeNamespaceReader) Node(_ context.Context, id int64) (namespace.Node, error) {
	node, ok := reader.nodes[id]
	if ok {
		return node, nil
	}
	if reader.global.ID == id {
		return reader.global, nil
	}
	for _, node := range reader.child {
		if node.ID == id {
			return node, nil
		}
	}
	for _, children := range reader.children {
		for _, node := range children {
			if node.ID == id {
				return node, nil
			}
		}
	}
	return namespace.Node{}, namespace.ErrNotFound
}

func (reader *fakeNamespaceReader) Child(_ context.Context, parentID int64, name string) (namespace.Node, error) {
	node, ok := reader.child[fakeChildKey(parentID, name)]
	if !ok {
		return namespace.Node{}, namespace.ErrNotFound
	}
	return node, nil
}

func (reader *fakeNamespaceReader) Children(_ context.Context, parentID int64) ([]namespace.Node, error) {
	children, ok := reader.children[parentID]
	if !ok {
		return nil, nil
	}
	return append([]namespace.Node(nil), children...), nil
}

func fakeChildKey(parentID int64, name string) string {
	return fmt.Sprintf("%d\x00%s", parentID, name)
}
