package fuse

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/sourcefile"
	"golang.org/x/sys/unix"
)

type projectionHandle struct {
	mu     sync.Mutex
	file   *sourcefile.File
	record namespace.Node
}

func (handle *projectionHandle) Read(
	_ context.Context,
	buffer []byte,
	offset int64,
) (gofuse.ReadResult, syscall.Errno) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return nil, syscall.EBADF
	}
	read, err := handle.file.ReadAt(buffer, offset)
	if read > 0 {
		return gofuse.ReadResultData(buffer[:read]), 0
	}
	if errors.Is(err, io.EOF) {
		return gofuse.ReadResultData(nil), 0
	}
	return nil, gofs.ToErrno(err)
}

func (handle *projectionHandle) Write(
	context.Context,
	[]byte,
	int64,
) (uint32, syscall.Errno) {
	return 0, syscall.EROFS
}

func (handle *projectionHandle) Release(context.Context) syscall.Errno {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return 0
	}
	err := handle.file.Close()
	handle.file = nil
	if errors.Is(err, os.ErrClosed) {
		return 0
	}
	return gofs.ToErrno(err)
}

func (handle *projectionHandle) Getattr(
	_ context.Context,
	out *gofuse.AttrOut,
) syscall.Errno {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return syscall.EBADF
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(handle.file.Fd()), &stat); err != nil {
		return gofs.ToErrno(err)
	}
	if err := fillProjectionAttr(
		handle.record,
		stat.Size,
		stat.Atim.Nano(),
		stat.Mtim.Nano(),
		&out.Attr,
	); err != nil {
		return syscall.EIO
	}
	return 0
}

func (handle *projectionHandle) Allocate(
	context.Context,
	uint64,
	uint64,
	uint32,
) syscall.Errno {
	return syscall.EOPNOTSUPP
}

var _ gofs.FileReader = (*projectionHandle)(nil)
var _ gofs.FileWriter = (*projectionHandle)(nil)
var _ gofs.FileReleaser = (*projectionHandle)(nil)
var _ gofs.FileGetattrer = (*projectionHandle)(nil)
var _ gofs.FileAllocater = (*projectionHandle)(nil)
