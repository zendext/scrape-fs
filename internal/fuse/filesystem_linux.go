package fuse

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

type namespaceReader interface {
	GlobalRoot(context.Context) (namespace.Node, error)
	Library(context.Context, string) (namespace.Library, error)
	Libraries(context.Context) ([]namespace.Library, error)
	Node(context.Context, int64) (namespace.Node, error)
	Child(context.Context, int64, string) (namespace.Node, error)
	Children(context.Context, int64) ([]namespace.Node, error)
}

type objectMetadataReader interface {
	StatObject(context.Context, int64) (unix.Stat_t, error)
}

type node struct {
	gofs.Inode
	reader  namespaceReader
	objects objectMetadataReader
	record  namespace.Node
}

func newRoot(
	ctx context.Context,
	reader namespaceReader,
	objects objectMetadataReader,
) (*node, error) {
	if reader == nil {
		return nil, errors.New("namespace reader is required")
	}
	if objects == nil {
		return nil, errors.New("object metadata reader is required")
	}
	root, err := reader.GlobalRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("read GlobalRoot: %w", err)
	}
	if root.ID != 1 || root.Kind != namespace.GlobalRoot || root.ParentID != nil {
		return nil, fmt.Errorf("%w: invalid GlobalRoot", namespace.ErrInvalidRoot)
	}
	return &node{reader: reader, objects: objects, record: root}, nil
}

func filesystemOptions(configuration config.FUSE) gofs.Options {
	return gofs.Options{
		MountOptions: gofuse.MountOptions{
			AllowOther:    true,
			Options:       []string{"default_permissions", "nodev", "nosuid", "noexec"},
			FsName:        filesystemName,
			Name:          filesystemName,
			DisableXAttrs: true,
		},
		EntryTimeout:    durationPointer(configuration.EntryTimeout),
		AttrTimeout:     durationPointer(configuration.AttrTimeout),
		NegativeTimeout: durationPointer(configuration.NegativeTimeout),
		RootStableAttr:  &gofs.StableAttr{Ino: 1},
		NullPermissions: true,
	}
}

func durationPointer(value time.Duration) *time.Duration {
	result := value
	return &result
}

func (current *node) Lookup(
	ctx context.Context,
	name string,
	out *gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	if !directoryKind(current.record.Kind) {
		return nil, syscall.ENOTDIR
	}

	var child namespace.Node
	var err error
	if current.record.Kind == namespace.GlobalRoot {
		var library namespace.Library
		library, err = current.reader.Library(ctx, name)
		if err == nil {
			child, err = current.reader.Node(ctx, library.RootNodeID)
		}
	} else {
		child, err = current.reader.Child(ctx, current.record.ID, name)
	}
	if err != nil {
		return nil, errorNumber(err)
	}
	if err := validateChild(current.record, child, name); err != nil {
		return nil, syscall.EIO
	}
	if err := fillAttr(ctx, child, current.objects, &out.Attr); err != nil {
		return nil, syscall.EIO
	}

	operations := &node{
		reader:  current.reader,
		objects: current.objects,
		record:  child,
	}
	inode := current.NewInode(ctx, operations, stableAttr(child))
	return inode, 0
}

func (current *node) Getattr(
	ctx context.Context,
	_ gofs.FileHandle,
	out *gofuse.AttrOut,
) syscall.Errno {
	record, err := current.reader.Node(ctx, current.record.ID)
	if err != nil {
		return errorNumber(err)
	}
	if record.ID != current.record.ID || record.Kind != current.record.Kind {
		return syscall.EIO
	}
	if err := fillAttr(ctx, record, current.objects, &out.Attr); err != nil {
		return syscall.EIO
	}
	return 0
}

func (current *node) Opendir(context.Context) syscall.Errno {
	if !directoryKind(current.record.Kind) {
		return syscall.ENOTDIR
	}
	return 0
}

func (current *node) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	if !directoryKind(current.record.Kind) {
		return nil, syscall.ENOTDIR
	}

	var children []namespace.Node
	if current.record.Kind == namespace.GlobalRoot {
		libraries, err := current.reader.Libraries(ctx)
		if err != nil {
			return nil, errorNumber(err)
		}
		children = make([]namespace.Node, 0, len(libraries))
		for _, library := range libraries {
			child, err := current.reader.Node(ctx, library.RootNodeID)
			if err != nil {
				return nil, errorNumber(err)
			}
			if err := validateChild(current.record, child, library.Name); err != nil {
				return nil, syscall.EIO
			}
			children = append(children, child)
		}
	} else {
		var err error
		children, err = current.reader.Children(ctx, current.record.ID)
		if err != nil {
			return nil, errorNumber(err)
		}
	}

	entries := make([]gofuse.DirEntry, len(children))
	for index, child := range children {
		if err := validateChild(current.record, child, child.Name); err != nil {
			return nil, syscall.EIO
		}
		entries[index] = gofuse.DirEntry{
			Name: child.Name,
			Ino:  uint64(child.ID),
			Mode: fileType(child.Kind),
		}
	}
	return gofs.NewListDirStream(entries), 0
}

func (current *node) Setattr(
	context.Context,
	gofs.FileHandle,
	*gofuse.SetAttrIn,
	*gofuse.AttrOut,
) syscall.Errno {
	return current.mutationError()
}

func (current *node) Mkdir(
	context.Context,
	string,
	uint32,
	*gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	return nil, current.mutationError()
}

func (current *node) Mknod(
	context.Context,
	string,
	uint32,
	uint32,
	*gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	return nil, current.mutationError()
}

func (current *node) Create(
	context.Context,
	string,
	uint32,
	uint32,
	*gofuse.EntryOut,
) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, current.mutationError()
}

func (current *node) Symlink(
	context.Context,
	string,
	string,
	*gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	return nil, current.mutationError()
}

func (current *node) Link(
	context.Context,
	gofs.InodeEmbedder,
	string,
	*gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	return nil, current.mutationError()
}

func (current *node) Unlink(context.Context, string) syscall.Errno {
	return current.mutationError()
}

func (current *node) Rmdir(context.Context, string) syscall.Errno {
	return current.mutationError()
}

func (current *node) Rename(
	context.Context,
	string,
	gofs.InodeEmbedder,
	string,
	uint32,
) syscall.Errno {
	return current.mutationError()
}

func (current *node) mutationError() syscall.Errno {
	if current.record.Kind == namespace.GlobalRoot ||
		current.record.Kind == namespace.LibraryRoot {
		return syscall.EPERM
	}
	return syscall.EROFS
}

func stableAttr(record namespace.Node) gofs.StableAttr {
	return gofs.StableAttr{
		Mode: fileType(record.Kind),
		Ino:  uint64(record.ID),
	}
}

func fillAttr(
	ctx context.Context,
	record namespace.Node,
	objects objectMetadataReader,
	attr *gofuse.Attr,
) error {
	mode := fileType(record.Kind)
	if mode == 0 || record.ID <= 0 {
		return namespace.ErrInvalidNode
	}

	attr.Ino = uint64(record.ID)
	attr.Mode = mode | uint32(record.Attributes.Mode.Perm())
	attr.Uid = record.Attributes.UID
	attr.Gid = record.Attributes.GID
	attr.Nlink = 1

	atime := record.AtimeNS
	mtime := record.MtimeNS
	ctime := record.VirtualCtimeNS
	switch record.Kind {
	case namespace.GlobalRoot, namespace.LibraryRoot, namespace.Directory:
		attr.Nlink = 2
	case namespace.ProjectionFile:
		if record.Source == nil || record.Source.Size < 0 {
			return namespace.ErrInvalidNode
		}
		attr.Size = uint64(record.Source.Size)
		atime = record.Source.AtimeNS
		mtime = record.Source.MtimeNS
		ctime = max(record.Source.SourceCtimeNS, record.VirtualCtimeNS)
	case namespace.OverlayFile:
		if record.ObjectID == nil || *record.ObjectID <= 0 {
			return namespace.ErrInvalidNode
		}
		if objects == nil {
			return namespace.ErrInvalidObject
		}
		stat, err := objects.StatObject(ctx, *record.ObjectID)
		if err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 0 {
			return namespace.ErrInvalidObject
		}
		attr.Size = uint64(stat.Size)
		atime = timespecNanoseconds(stat.Atim)
		mtime = timespecNanoseconds(stat.Mtim)
		ctime = max(timespecNanoseconds(stat.Ctim), record.VirtualCtimeNS)
	}

	attr.Atime, attr.Atimensec = splitNanoseconds(atime)
	attr.Mtime, attr.Mtimensec = splitNanoseconds(mtime)
	attr.Ctime, attr.Ctimensec = splitNanoseconds(ctime)
	return nil
}

func timespecNanoseconds(value unix.Timespec) int64 {
	return value.Sec*int64(time.Second) + value.Nsec
}

func splitNanoseconds(value int64) (uint64, uint32) {
	seconds := value / int64(time.Second)
	nanoseconds := value % int64(time.Second)
	if nanoseconds < 0 {
		seconds--
		nanoseconds += int64(time.Second)
	}
	return uint64(seconds), uint32(nanoseconds)
}

func validateChild(parent, child namespace.Node, name string) error {
	if child.ID <= 0 ||
		child.ParentID == nil ||
		*child.ParentID != parent.ID ||
		child.Name != name {
		return namespace.ErrInvalidNode
	}
	if parent.Kind == namespace.GlobalRoot {
		if child.Kind != namespace.LibraryRoot {
			return namespace.ErrInvalidNode
		}
		return nil
	}
	if parent.Kind != namespace.LibraryRoot && parent.Kind != namespace.Directory {
		return namespace.ErrInvalidNode
	}
	if child.Kind == namespace.GlobalRoot || child.Kind == namespace.LibraryRoot {
		return namespace.ErrInvalidNode
	}
	return nil
}

func directoryKind(kind namespace.NodeKind) bool {
	return kind == namespace.GlobalRoot ||
		kind == namespace.LibraryRoot ||
		kind == namespace.Directory
}

func fileType(kind namespace.NodeKind) uint32 {
	if directoryKind(kind) {
		return syscall.S_IFDIR
	}
	if kind == namespace.ProjectionFile || kind == namespace.OverlayFile {
		return syscall.S_IFREG
	}
	return 0
}

func errorNumber(err error) syscall.Errno {
	switch {
	case errors.Is(err, namespace.ErrNotFound),
		errors.Is(err, namespace.ErrLibraryNotFound),
		errors.Is(err, namespace.ErrInvalidPath):
		return syscall.ENOENT
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return syscall.EINTR
	default:
		return syscall.EIO
	}
}
