package fuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"strings"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/config"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
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

type namespaceWriter interface {
	Update(
		context.Context,
		namespace.Operation,
		func(*namespace.Transaction) error,
	) (int64, error)
	SetAttributes(context.Context, namespace.SetAttributes) (namespace.Node, error)
}

type objectWriter interface {
	CreateWithFlags(
		context.Context,
		namespace.Operation,
		namespace.CreateOverlay,
		io.Reader,
		int,
	) (*objectstore.File, namespace.Node, error)
	OpenObject(context.Context, int64, int) (*objectstore.File, error)
	DeleteObject(int64) error
}

type node struct {
	gofs.Inode
	reader       namespaceReader
	writer       namespaceWriter
	objects      objectMetadataReader
	objectWriter objectWriter
	record       namespace.Node
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
	writer, _ := reader.(namespaceWriter)
	objectWriter, _ := objects.(objectWriter)
	return &node{
		reader:       reader,
		writer:       writer,
		objects:      objects,
		objectWriter: objectWriter,
		record:       root,
	}, nil
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
		reader:       current.reader,
		writer:       current.writer,
		objects:      current.objects,
		objectWriter: current.objectWriter,
		record:       child,
	}
	inode := current.NewInode(ctx, operations, stableAttr(child))
	return inode, 0
}

func (current *node) Getattr(
	ctx context.Context,
	file gofs.FileHandle,
	out *gofuse.AttrOut,
) syscall.Errno {
	record, err := current.reader.Node(ctx, current.record.ID)
	if err != nil {
		if handle, ok := file.(*overlayHandle); ok &&
			errors.Is(err, namespace.ErrNotFound) {
			return handle.Getattr(ctx, out)
		}
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
	ctx context.Context,
	file gofs.FileHandle,
	input *gofuse.SetAttrIn,
	out *gofuse.AttrOut,
) syscall.Errno {
	switch current.record.Kind {
	case namespace.GlobalRoot, namespace.LibraryRoot:
		return syscall.EPERM
	case namespace.ProjectionFile:
		return syscall.EROFS
	case namespace.Directory, namespace.OverlayFile:
	default:
		return syscall.EIO
	}
	if current.writer == nil {
		return syscall.EIO
	}

	modeValue, hasMode := input.GetMode()
	uid, hasUID := input.GetUID()
	gid, hasGID := input.GetGID()
	size, hasSize := input.GetSize()
	atime, hasAtime := input.GetATime()
	mtime, hasMtime := input.GetMTime()
	if current.record.Kind == namespace.Directory && hasSize {
		return syscall.EISDIR
	}

	attributes := namespace.SetAttributes{NodeID: current.record.ID}
	if hasMode {
		mode := fs.FileMode(modeValue).Perm()
		attributes.Mode = &mode
	}
	if hasUID {
		attributes.UID = &uid
	}
	if hasGID {
		attributes.GID = &gid
	}
	if current.record.Kind == namespace.Directory {
		if hasAtime {
			value := atime.UnixNano()
			attributes.AtimeNS = &value
		}
		if hasMtime {
			value := mtime.UnixNano()
			attributes.MtimeNS = &value
		}
	}

	requestHandle, handleBased := file.(*overlayHandle)
	objectChanged := false
	if current.record.Kind == namespace.OverlayFile &&
		(hasSize || hasAtime || hasMtime) {
		handle, temporary, errno := current.overlayHandle(ctx, file, unix.O_RDWR)
		if errno != 0 {
			return errno
		}
		if temporary {
			defer handle.Release(context.Background())
		}
		if hasSize {
			if size > math.MaxInt64 {
				return syscall.EFBIG
			}
			if errno := handle.truncate(int64(size)); errno != 0 {
				return errno
			}
		}
		if hasAtime || hasMtime {
			var atimePointer *time.Time
			var mtimePointer *time.Time
			if hasAtime {
				atimePointer = &atime
			}
			if hasMtime {
				mtimePointer = &mtime
			}
			if errno := handle.setTimes(atimePointer, mtimePointer); errno != 0 {
				return errno
			}
		}
		objectChanged = true
	}

	virtualChanged := attributes.Mode != nil ||
		attributes.UID != nil ||
		attributes.GID != nil
	directoryTimesChanged := attributes.AtimeNS != nil ||
		attributes.MtimeNS != nil
	metadataChanged := virtualChanged ||
		directoryTimesChanged ||
		objectChanged
	persistMetadata := (current.record.Kind == namespace.Directory &&
		(virtualChanged || directoryTimesChanged)) ||
		(current.record.Kind == namespace.OverlayFile && virtualChanged)
	if persistMetadata {
		record, err := current.writer.SetAttributes(ctx, attributes)
		if err != nil {
			if current.record.Kind == namespace.OverlayFile &&
				handleBased &&
				errors.Is(err, namespace.ErrNotFound) {
				requestHandle.updateVirtualAttributes(attributes)
			} else {
				return errorNumber(err)
			}
		} else if handleBased {
			requestHandle.updateRecord(record)
		}
	}
	if errno := current.Getattr(ctx, file, out); errno != 0 {
		return errno
	}
	if metadataChanged {
		_ = current.NotifyContent(0, 0)
	}
	return 0
}

func (current *node) Mkdir(
	ctx context.Context,
	name string,
	mode uint32,
	out *gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	if current.record.Kind == namespace.GlobalRoot {
		return nil, syscall.EPERM
	}
	if !directoryKind(current.record.Kind) {
		return nil, syscall.ENOTDIR
	}
	caller, actor, errno := fuseActor(ctx)
	if errno != 0 || current.writer == nil {
		if errno == 0 {
			errno = syscall.EIO
		}
		return nil, errno
	}
	var created namespace.Node
	operation := current.operation(ctx, "fuse.directory.create", name, namespace.Directory, actor)
	if _, err := current.writer.Update(ctx, operation, func(transaction *namespace.Transaction) error {
		var err error
		created, err = transaction.CreateNode(ctx, namespace.CreateNode{
			ParentID: current.record.ID,
			Name:     name,
			Kind:     namespace.Directory,
			Creator:  actor,
			Attributes: namespace.Attributes{
				Mode: fs.FileMode(mode).Perm(),
				UID:  caller.Uid,
				GID:  caller.Gid,
			},
		})
		return err
	}); err != nil {
		return nil, errorNumber(err)
	}
	child, errno := current.newChild(ctx, created, out)
	if errno == 0 {
		current.invalidateAttributes()
	}
	return child, errno
}

func (current *node) Mknod(
	context.Context,
	string,
	uint32,
	uint32,
	*gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	if current.record.Kind == namespace.GlobalRoot {
		return nil, syscall.EPERM
	}
	if !directoryKind(current.record.Kind) {
		return nil, syscall.ENOTDIR
	}
	return nil, syscall.EOPNOTSUPP
}

func (current *node) Create(
	ctx context.Context,
	name string,
	flags uint32,
	mode uint32,
	out *gofuse.EntryOut,
) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	if current.record.Kind == namespace.GlobalRoot {
		return nil, nil, 0, syscall.EPERM
	}
	if !directoryKind(current.record.Kind) {
		return nil, nil, 0, syscall.ENOTDIR
	}
	caller, actor, errno := fuseActor(ctx)
	if errno != 0 || current.objectWriter == nil {
		if errno == 0 {
			errno = syscall.EIO
		}
		return nil, nil, 0, errno
	}
	operation := current.operation(ctx, "fuse.overlay.create", name, namespace.OverlayFile, actor)
	file, created, err := current.objectWriter.CreateWithFlags(
		ctx,
		operation,
		namespace.CreateOverlay{
			ParentID: current.record.ID,
			Name:     name,
			Creator:  actor,
			Attributes: namespace.Attributes{
				Mode: fs.FileMode(mode).Perm(),
				UID:  caller.Uid,
				GID:  caller.Gid,
			},
		},
		nil,
		int(flags),
	)
	if err != nil {
		return nil, nil, 0, errorNumber(err)
	}
	inode, errno := current.newChild(ctx, created, out)
	if errno != 0 {
		file.Close()
		return nil, nil, 0, errno
	}
	current.invalidateAttributes()
	handle := &overlayHandle{file: file, flags: int(flags), record: created}
	return inode, handle, 0, 0
}

func (current *node) Symlink(
	context.Context,
	string,
	string,
	*gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	if current.record.Kind == namespace.GlobalRoot {
		return nil, syscall.EPERM
	}
	if !directoryKind(current.record.Kind) {
		return nil, syscall.ENOTDIR
	}
	return nil, syscall.EOPNOTSUPP
}

func (current *node) Link(
	context.Context,
	gofs.InodeEmbedder,
	string,
	*gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	if current.record.Kind == namespace.GlobalRoot {
		return nil, syscall.EPERM
	}
	if !directoryKind(current.record.Kind) {
		return nil, syscall.ENOTDIR
	}
	return nil, syscall.EOPNOTSUPP
}

func (current *node) Readlink(context.Context) ([]byte, syscall.Errno) {
	return nil, syscall.EOPNOTSUPP
}

func (current *node) Unlink(ctx context.Context, name string) syscall.Errno {
	if current.record.Kind == namespace.GlobalRoot {
		return syscall.EPERM
	}
	if !directoryKind(current.record.Kind) {
		return syscall.ENOTDIR
	}
	if current.writer == nil || current.objectWriter == nil {
		return syscall.EIO
	}
	child, err := current.reader.Child(ctx, current.record.ID, name)
	if err != nil {
		return errorNumber(err)
	}
	if child.Kind == namespace.Directory {
		return syscall.EISDIR
	}
	if child.Kind != namespace.OverlayFile {
		return syscall.EROFS
	}
	_, actor, errno := fuseActor(ctx)
	if errno != 0 {
		return errno
	}
	operation := current.operation(ctx, "fuse.overlay.unlink", name, child.Kind, actor)
	var removed namespace.Node
	if _, err := current.writer.Update(ctx, operation, func(transaction *namespace.Transaction) error {
		var err error
		removed, err = transaction.RemoveOverlay(ctx, current.record.ID, name)
		return err
	}); err != nil {
		return errorNumber(err)
	}
	if removed.ObjectID != nil {
		_ = current.objectWriter.DeleteObject(*removed.ObjectID)
	}
	current.invalidateAttributes()
	return 0
}

func (current *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if current.record.Kind == namespace.GlobalRoot {
		return syscall.EPERM
	}
	if !directoryKind(current.record.Kind) {
		return syscall.ENOTDIR
	}
	if current.writer == nil {
		return syscall.EIO
	}
	child, err := current.reader.Child(ctx, current.record.ID, name)
	if err != nil {
		return errorNumber(err)
	}
	if child.Kind != namespace.Directory {
		return syscall.ENOTDIR
	}
	_, actor, errno := fuseActor(ctx)
	if errno != 0 {
		return errno
	}
	operation := current.operation(ctx, "fuse.directory.remove", name, child.Kind, actor)
	if _, err := current.writer.Update(ctx, operation, func(transaction *namespace.Transaction) error {
		_, err := transaction.RemoveDirectory(ctx, current.record.ID, name)
		return err
	}); err != nil {
		return errorNumber(err)
	}
	current.invalidateAttributes()
	return 0
}

func (current *node) Rename(
	ctx context.Context,
	name string,
	newParent gofs.InodeEmbedder,
	newName string,
	flags uint32,
) syscall.Errno {
	if current.record.Kind == namespace.GlobalRoot {
		return syscall.EPERM
	}
	if flags != 0 && flags != unix.RENAME_NOREPLACE {
		return syscall.EINVAL
	}
	targetParent, ok := newParent.(*node)
	if !ok ||
		current.writer == nil ||
		targetParent.writer == nil ||
		current.writer != targetParent.writer {
		return syscall.EXDEV
	}
	source, err := current.reader.Child(ctx, current.record.ID, name)
	if err != nil {
		return errorNumber(err)
	}
	if source.Kind == namespace.ProjectionFile {
		return syscall.EROFS
	}
	if source.Kind != namespace.Directory && source.Kind != namespace.OverlayFile {
		return syscall.EPERM
	}
	sourceInode := current.GetChild(name)
	_, actor, errno := fuseActor(ctx)
	if errno != 0 {
		return errno
	}
	operation := namespace.Operation{
		Name:  "fuse.node.rename",
		Actor: actor,
		Details: namespace.OperationDetails{
			Paths: []string{
				current.childPath(ctx, name),
				targetParent.childPath(ctx, newName),
			},
			NodeKinds: []namespace.NodeKind{source.Kind, source.Kind},
		},
	}
	var result namespace.RenameResult
	if _, err := current.writer.Update(ctx, operation, func(transaction *namespace.Transaction) error {
		var err error
		result, err = transaction.Rename(ctx, namespace.RenameNode{
			OldParentID:        current.record.ID,
			OldName:            name,
			NewParentID:        targetParent.record.ID,
			NewName:            newName,
			NoReplace:          flags == unix.RENAME_NOREPLACE,
			ExpectedSourceID:   source.ID,
			ExpectedSourceKind: source.Kind,
		})
		return err
	}); err != nil {
		return errorNumber(err)
	}
	if result.Replaced != nil &&
		result.Replaced.ObjectID != nil &&
		current.objectWriter != nil {
		_ = current.objectWriter.DeleteObject(*result.Replaced.ObjectID)
	}
	current.invalidateAttributes()
	targetParent.invalidateAttributes()
	if sourceInode != nil {
		_ = sourceInode.NotifyContent(0, 0)
	}
	return 0
}

func (current *node) Open(
	ctx context.Context,
	flags uint32,
) (gofs.FileHandle, uint32, syscall.Errno) {
	if directoryKind(current.record.Kind) {
		return nil, 0, syscall.EISDIR
	}
	if current.record.Kind == namespace.ProjectionFile {
		if flags&(unix.O_WRONLY|unix.O_RDWR|unix.O_APPEND|unix.O_TRUNC) != 0 {
			return nil, 0, syscall.EROFS
		}
		return nil, 0, syscall.EOPNOTSUPP
	}
	if current.record.Kind != namespace.OverlayFile ||
		current.objectWriter == nil {
		return nil, 0, syscall.EIO
	}
	if flags&unix.O_CREAT != 0 && flags&unix.O_EXCL != 0 {
		return nil, 0, syscall.EEXIST
	}
	record, err := current.reader.Node(ctx, current.record.ID)
	if err != nil {
		return nil, 0, errorNumber(err)
	}
	if record.ObjectID == nil {
		return nil, 0, syscall.EIO
	}
	backingFlags := int(flags &^ gofuse.FMODE_EXEC)
	file, err := current.objectWriter.OpenObject(
		ctx,
		*record.ObjectID,
		backingFlags&^(unix.O_CREAT|unix.O_EXCL),
	)
	if err != nil {
		return nil, 0, errorNumber(err)
	}
	if flags&unix.O_TRUNC != 0 {
		_ = current.NotifyContent(0, 0)
	}
	return &overlayHandle{file: file, flags: int(flags), record: record}, 0, 0
}

func (current *node) Allocate(
	context.Context,
	gofs.FileHandle,
	uint64,
	uint64,
	uint32,
) syscall.Errno {
	return syscall.EOPNOTSUPP
}

func (current *node) newChild(
	ctx context.Context,
	record namespace.Node,
	out *gofuse.EntryOut,
) (*gofs.Inode, syscall.Errno) {
	if err := validateChild(current.record, record, record.Name); err != nil {
		return nil, syscall.EIO
	}
	if err := fillAttr(ctx, record, current.objects, &out.Attr); err != nil {
		return nil, errorNumber(err)
	}
	operations := &node{
		reader:       current.reader,
		writer:       current.writer,
		objects:      current.objects,
		objectWriter: current.objectWriter,
		record:       record,
	}
	return current.NewInode(ctx, operations, stableAttr(record)), 0
}

func (current *node) overlayHandle(
	ctx context.Context,
	file gofs.FileHandle,
	flags int,
) (*overlayHandle, bool, syscall.Errno) {
	if file != nil {
		handle, ok := file.(*overlayHandle)
		if !ok {
			return nil, false, syscall.EIO
		}
		return handle, false, 0
	}
	if current.objectWriter == nil {
		return nil, false, syscall.EIO
	}
	record, err := current.reader.Node(ctx, current.record.ID)
	if err != nil {
		return nil, false, errorNumber(err)
	}
	if record.Kind != namespace.OverlayFile ||
		record.ObjectID == nil ||
		*record.ObjectID <= 0 {
		return nil, false, syscall.EIO
	}
	opened, err := current.objectWriter.OpenObject(ctx, *record.ObjectID, flags)
	if err != nil {
		return nil, false, errorNumber(err)
	}
	return &overlayHandle{file: opened, flags: flags, record: record}, true, 0
}

func (current *node) operation(
	ctx context.Context,
	name string,
	childName string,
	kind namespace.NodeKind,
	actor namespace.Actor,
) namespace.Operation {
	return namespace.Operation{
		Name:  name,
		Actor: actor,
		Details: namespace.OperationDetails{
			Paths:     []string{current.childPath(ctx, childName)},
			NodeKinds: []namespace.NodeKind{kind},
		},
	}
}

func (current *node) childPath(ctx context.Context, name string) string {
	components := []string{name}
	seen := map[int64]struct{}{}
	currentID := current.record.ID
	for currentID != 1 {
		if _, duplicate := seen[currentID]; duplicate {
			return fmt.Sprintf("inode:%d/%s", current.record.ID, name)
		}
		seen[currentID] = struct{}{}
		record, err := current.reader.Node(ctx, currentID)
		if err != nil || record.ParentID == nil {
			return fmt.Sprintf("inode:%d/%s", current.record.ID, name)
		}
		components = append(components, record.Name)
		currentID = *record.ParentID
	}
	for left, right := 0, len(components)-1; left < right; left, right = left+1, right-1 {
		components[left], components[right] = components[right], components[left]
	}
	return strings.Join(components, "/")
}

func (current *node) invalidateAttributes() {
	_ = current.NotifyContent(0, 0)
}

func fuseActor(
	ctx context.Context,
) (*gofuse.Caller, namespace.Actor, syscall.Errno) {
	caller, ok := gofuse.FromContext(ctx)
	if !ok {
		return nil, namespace.Actor{}, syscall.EIO
	}
	actorID := int64(caller.Uid)
	return caller, namespace.Actor{
		Kind: namespace.ActorFuse,
		ID:   &actorID,
	}, 0
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
		return fillOverlayAttr(record, stat, attr)
	}

	attr.Atime, attr.Atimensec = splitNanoseconds(atime)
	attr.Mtime, attr.Mtimensec = splitNanoseconds(mtime)
	attr.Ctime, attr.Ctimensec = splitNanoseconds(ctime)
	return nil
}

func fillOverlayAttr(
	record namespace.Node,
	stat unix.Stat_t,
	attr *gofuse.Attr,
) error {
	if record.ID <= 0 ||
		record.Kind != namespace.OverlayFile ||
		record.ObjectID == nil ||
		*record.ObjectID <= 0 ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Size < 0 {
		return namespace.ErrInvalidObject
	}
	attr.Ino = uint64(record.ID)
	attr.Mode = syscall.S_IFREG | uint32(record.Attributes.Mode.Perm())
	attr.Uid = record.Attributes.UID
	attr.Gid = record.Attributes.GID
	attr.Nlink = 1
	attr.Size = uint64(stat.Size)
	attr.Atime, attr.Atimensec = splitNanoseconds(timespecNanoseconds(stat.Atim))
	attr.Mtime, attr.Mtimensec = splitNanoseconds(timespecNanoseconds(stat.Mtim))
	attr.Ctime, attr.Ctimensec = splitNanoseconds(max(
		timespecNanoseconds(stat.Ctim),
		record.VirtualCtimeNS,
	))
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
	case err == nil:
		return 0
	case errors.Is(err, syscall.EPERM):
		return syscall.EPERM
	case errors.Is(err, syscall.EACCES):
		return syscall.EACCES
	case errors.Is(err, syscall.EBADF):
		return syscall.EBADF
	case errors.Is(err, syscall.EINVAL):
		return syscall.EINVAL
	case errors.Is(err, syscall.ENOSPC):
		return syscall.ENOSPC
	case errors.Is(err, syscall.EFBIG):
		return syscall.EFBIG
	case errors.Is(err, syscall.EIO):
		return syscall.EIO
	case errors.Is(err, namespace.ErrConflict):
		return syscall.EEXIST
	case errors.Is(err, namespace.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, namespace.ErrCrossLibrary):
		return syscall.EXDEV
	case errors.Is(err, namespace.ErrProtected):
		return syscall.EPERM
	case errors.Is(err, namespace.ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, namespace.ErrIsDirectory):
		return syscall.EISDIR
	case errors.Is(err, namespace.ErrNotDirectory):
		return syscall.ENOTDIR
	case errors.Is(err, namespace.ErrInvalidOperation):
		return syscall.EINVAL
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
