package fuse

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/objectstore"
	"golang.org/x/sys/unix"
)

type overlayHandle struct {
	mu     sync.Mutex
	file   *objectstore.File
	flags  int
	record namespace.Node
}

func (handle *overlayHandle) Read(
	_ context.Context,
	buffer []byte,
	offset int64,
) (gofuse.ReadResult, syscall.Errno) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return nil, syscall.EBADF
	}
	if handle.flags&unix.O_ACCMODE == unix.O_WRONLY {
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

func (handle *overlayHandle) Write(
	_ context.Context,
	data []byte,
	offset int64,
) (uint32, syscall.Errno) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return 0, syscall.EBADF
	}
	if handle.flags&unix.O_ACCMODE == unix.O_RDONLY {
		return 0, syscall.EBADF
	}

	var written int
	var err error
	if handle.flags&unix.O_APPEND != 0 {
		written, err = handle.file.Write(data)
	} else {
		written, err = handle.file.WriteAt(data, offset)
	}
	if written > 0 {
		return uint32(written), 0
	}
	return 0, gofs.ToErrno(err)
}

func (handle *overlayHandle) Flush(context.Context) syscall.Errno {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return syscall.EBADF
	}
	duplicate, err := unix.Dup(int(handle.file.Fd()))
	if err != nil {
		return gofs.ToErrno(err)
	}
	return gofs.ToErrno(unix.Close(duplicate))
}

func (handle *overlayHandle) Fsync(
	_ context.Context,
	flags uint32,
) syscall.Errno {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return syscall.EBADF
	}
	if flags&1 != 0 {
		return gofs.ToErrno(unix.Fdatasync(int(handle.file.Fd())))
	}
	return gofs.ToErrno(unix.Fsync(int(handle.file.Fd())))
}

func (handle *overlayHandle) Release(context.Context) syscall.Errno {
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

func (handle *overlayHandle) Allocate(
	context.Context,
	uint64,
	uint64,
	uint32,
) syscall.Errno {
	return syscall.EOPNOTSUPP
}

func (handle *overlayHandle) Getattr(
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
	if err := fillOverlayAttr(handle.record, stat, &out.Attr); err != nil {
		return syscall.EIO
	}
	return 0
}

func (handle *overlayHandle) updateRecord(record namespace.Node) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	handle.record = record
}

func (handle *overlayHandle) truncate(size int64) syscall.Errno {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return syscall.EBADF
	}
	if handle.flags&unix.O_ACCMODE == unix.O_RDONLY {
		return syscall.EBADF
	}
	return gofs.ToErrno(handle.file.Truncate(size))
}

func (handle *overlayHandle) setTimes(
	atime *time.Time,
	mtime *time.Time,
) syscall.Errno {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.file == nil {
		return syscall.EBADF
	}
	times := []unix.Timespec{
		{Nsec: unix.UTIME_OMIT},
		{Nsec: unix.UTIME_OMIT},
	}
	if atime != nil {
		times[0] = unix.NsecToTimespec(atime.UnixNano())
	}
	if mtime != nil {
		times[1] = unix.NsecToTimespec(mtime.UnixNano())
	}
	return gofs.ToErrno(unix.UtimesNanoAt(
		int(handle.file.Fd()),
		"",
		times,
		unix.AT_EMPTY_PATH,
	))
}

var _ gofs.FileReader = (*overlayHandle)(nil)
var _ gofs.FileWriter = (*overlayHandle)(nil)
var _ gofs.FileFlusher = (*overlayHandle)(nil)
var _ gofs.FileFsyncer = (*overlayHandle)(nil)
var _ gofs.FileReleaser = (*overlayHandle)(nil)
var _ gofs.FileAllocater = (*overlayHandle)(nil)
var _ gofs.FileGetattrer = (*overlayHandle)(nil)
