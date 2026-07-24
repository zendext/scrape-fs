package objectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

const (
	stateStaging             = "STAGING"
	stateLive                = "LIVE"
	defaultOrphanGracePeriod = 24 * time.Hour
)

var ErrClosed = errors.New("object store is closed")

type Options struct {
	OrphanGracePeriod time.Duration
	Now               func() time.Time
	Repository        *namespace.Repository
}

type Store struct {
	database    *database.DB
	repository  *namespace.Repository
	root        *os.File
	staging     *os.File
	live        *os.File
	now         func() time.Time
	gracePeriod time.Duration
	active      map[int64]int
	recovery    RecoveryReport
	coordinator sync.Mutex
	closeOnce   sync.Once
	closeErr    error
	closed      bool
	hooks       hooks
}

type File struct {
	*os.File
	ObjectID int64
	store    *Store
	closeMu  sync.Mutex
	closed   bool
}

func (file *File) Close() error {
	file.closeMu.Lock()
	defer file.closeMu.Unlock()
	if file.closed {
		return os.ErrClosed
	}
	file.closed = true
	err := file.File.Close()
	if file.store != nil {
		file.store.releaseHandle(file.ObjectID)
	}
	return err
}

func (file *File) EnableAppend() error {
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("read object %d flags: %w", file.ObjectID, err)
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFL, flags|unix.O_APPEND); err != nil {
		return fmt.Errorf("enable append for object %d: %w", file.ObjectID, err)
	}
	return nil
}

type hooks struct {
	afterStagingRecord func(int64) error
	afterStagingSync   func(int64) error
	afterRename        func(int64) error
	prepareHandle      func(*File) error
	beforeDelete       func(int64) error
}

func Open(path string, opened *database.DB) (_ *Store, err error) {
	return OpenWithOptions(path, opened, Options{})
}

func OpenWithOptions(path string, opened *database.DB, options Options) (_ *Store, err error) {
	if opened == nil {
		return nil, fmt.Errorf("open object store: database must not be nil")
	}
	if options.OrphanGracePeriod == 0 {
		options.OrphanGracePeriod = defaultOrphanGracePeriod
	}
	if options.OrphanGracePeriod < 0 {
		return nil, fmt.Errorf("open object store: orphan grace period must be positive")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	repository := options.Repository
	if repository == nil {
		repository = namespace.NewRepository(opened)
	}
	root, err := prepareRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open object store %q: %w", path, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, root.Close())
		}
	}()

	staging, err := openPrivateSubdirectory(root, stagingDirectory)
	if err != nil {
		return nil, fmt.Errorf("open object store %q: %w", path, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, staging.Close())
		}
	}()
	live, err := openPrivateSubdirectory(root, liveDirectory)
	if err != nil {
		return nil, fmt.Errorf("open object store %q: %w", path, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, live.Close())
		}
	}()

	if err := validateStoredObjects(staging, stagingDirectory); err != nil {
		return nil, fmt.Errorf("open object store %q: %w", path, err)
	}
	if err := validateStoredObjects(live, liveDirectory); err != nil {
		return nil, fmt.Errorf("open object store %q: %w", path, err)
	}
	store := &Store{
		database:    opened,
		repository:  repository,
		root:        root,
		staging:     staging,
		live:        live,
		now:         options.Now,
		gracePeriod: options.OrphanGracePeriod,
		active:      make(map[int64]int),
	}
	report, err := store.recover(context.Background())
	if err != nil {
		return nil, fmt.Errorf("recover object store %q: %w", path, err)
	}
	store.recovery = report
	return store, nil
}

func (store *Store) Create(
	ctx context.Context,
	operation namespace.Operation,
	request namespace.CreateOverlay,
	initial io.Reader,
) (_ *File, _ namespace.Node, err error) {
	return store.CreateWithFlags(ctx, operation, request, initial, 0)
}

func (store *Store) CreateWithFlags(
	ctx context.Context,
	operation namespace.Operation,
	request namespace.CreateOverlay,
	initial io.Reader,
	flags int,
) (_ *File, _ namespace.Node, err error) {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	if store.closed {
		return nil, namespace.Node{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, namespace.Node{}, err
	}

	objectID, err := store.createStagingRecord(ctx)
	if err != nil {
		return nil, namespace.Node{}, err
	}
	if err := runHook(store.hooks.afterStagingRecord, objectID); err != nil {
		return nil, namespace.Node{}, err
	}

	raw, err := createStagingFile(store.staging, objectID)
	if err != nil {
		return nil, namespace.Node{}, err
	}
	handle := &File{File: raw, ObjectID: objectID}
	defer func() {
		if err != nil {
			err = errors.Join(err, handle.Close())
		}
	}()

	if initial != nil {
		if _, err := io.Copy(handle, initial); err != nil {
			return nil, namespace.Node{}, fmt.Errorf("write staging object %d: %w", objectID, err)
		}
	}
	if err := handle.Sync(); err != nil {
		return nil, namespace.Node{}, fmt.Errorf("fsync staging object %d: %w", objectID, err)
	}
	if err := runHook(store.hooks.afterStagingSync, objectID); err != nil {
		return nil, namespace.Node{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, namespace.Node{}, err
	}

	name := strconv.FormatInt(objectID, 10)
	if err := unix.Renameat2(
		int(store.staging.Fd()),
		name,
		int(store.live.Fd()),
		name,
		unix.RENAME_NOREPLACE,
	); err != nil {
		return nil, namespace.Node{}, fmt.Errorf("promote object %d to live: %w", objectID, err)
	}
	if err := runHook(store.hooks.afterRename, objectID); err != nil {
		return nil, namespace.Node{}, err
	}
	if err := store.staging.Sync(); err != nil {
		return nil, namespace.Node{}, fmt.Errorf("fsync staging directory: %w", err)
	}
	if err := store.live.Sync(); err != nil {
		return nil, namespace.Node{}, fmt.Errorf("fsync live directory: %w", err)
	}
	if err := store.prepareHandle(handle); err != nil {
		return nil, namespace.Node{}, err
	}
	if flags&unix.O_APPEND != 0 {
		if err := handle.EnableAppend(); err != nil {
			return nil, namespace.Node{}, err
		}
	}

	var node namespace.Node
	if _, err := store.repository.Update(ctx, operation, func(transaction *namespace.Transaction) error {
		var createErr error
		node, createErr = transaction.CreateOverlay(ctx, objectID, request)
		return createErr
	}); err != nil {
		return nil, namespace.Node{}, err
	}
	store.trackHandle(handle)
	return handle, node, nil
}

func (store *Store) OpenObject(
	ctx context.Context,
	objectID int64,
	flags int,
) (*File, error) {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	if store.closed {
		return nil, ErrClosed
	}
	if objectID <= 0 {
		return nil, fmt.Errorf("open object %d: %w", objectID, unix.EIO)
	}
	if flags&(unix.O_CREAT|unix.O_EXCL|unix.O_TMPFILE) != 0 {
		return nil, fmt.Errorf("open object %d: creation flags are not allowed: %w", objectID, unix.EINVAL)
	}
	if accessMode := flags & unix.O_ACCMODE; accessMode != unix.O_RDONLY &&
		accessMode != unix.O_WRONLY &&
		accessMode != unix.O_RDWR {
		return nil, fmt.Errorf("open object %d: invalid access mode: %w", objectID, unix.EINVAL)
	}
	live, err := store.objectIsLive(ctx, objectID)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, fmt.Errorf("open object %d: object is not LIVE: %w", objectID, unix.EIO)
	}

	truncate := flags&unix.O_TRUNC != 0
	file, err := openExistingObject(store.live, objectID, flags&^unix.O_TRUNC)
	if err != nil {
		return nil, fmt.Errorf("open live object %d: %v: %w", objectID, err, unix.EIO)
	}
	handle := &File{File: file, ObjectID: objectID}
	if truncate {
		if err := handle.Truncate(0); err != nil {
			return nil, errors.Join(
				fmt.Errorf("truncate live object %d: %w", objectID, err),
				handle.Close(),
			)
		}
	}
	store.trackHandle(handle)
	return handle, nil
}

func (store *Store) StatObject(
	ctx context.Context,
	objectID int64,
) (_ unix.Stat_t, err error) {
	file, err := store.OpenObject(ctx, objectID, unix.O_RDONLY)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return unix.Stat_t{}, fmt.Errorf("stat live object %d: %w", objectID, err)
	}
	return stat, nil
}

func (store *Store) DeleteObject(objectID int64) error {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	if store.closed {
		return ErrClosed
	}
	if objectID <= 0 {
		return fmt.Errorf("delete object %d: %w", objectID, unix.EINVAL)
	}
	_, err := store.reclaimRecord(context.Background(), objectID, true)
	return err
}

func (store *Store) Close() error {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	store.closeOnce.Do(func() {
		store.closed = true
		store.closeErr = errors.Join(
			store.staging.Close(),
			store.live.Close(),
			store.root.Close(),
		)
	})
	return store.closeErr
}

func (store *Store) createStagingRecord(ctx context.Context) (int64, error) {
	now := store.now().UTC().UnixNano()
	var objectID int64
	err := store.database.UseConnection(ctx, func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin STAGING object transaction: %w", err)
		}
		defer transaction.Rollback()
		result, err := transaction.ExecContext(ctx, `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES ('STAGING', ?, NULL, ?)
		`, now, now)
		if err != nil {
			return fmt.Errorf("create STAGING object: %w", err)
		}
		objectID, err = result.LastInsertId()
		if err != nil {
			return fmt.Errorf("read STAGING object ID: %w", err)
		}
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit STAGING object: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return objectID, nil
}

func (store *Store) objectIsLive(ctx context.Context, objectID int64) (bool, error) {
	var state string
	err := store.database.UseConnection(ctx, func(connection *database.Connection) error {
		return connection.QueryRowContext(
			ctx,
			"SELECT state FROM objects WHERE id = ?",
			objectID,
		).Scan(&state)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read object %d state: %w", objectID, err)
	}
	return state == stateLive, nil
}

func runHook(hook func(int64) error, objectID int64) error {
	if hook == nil {
		return nil
	}
	return hook(objectID)
}

func (store *Store) prepareHandle(handle *File) error {
	if store.hooks.prepareHandle != nil {
		return store.hooks.prepareHandle(handle)
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind live object %d: %w", handle.ObjectID, err)
	}
	return nil
}

func (store *Store) trackHandle(handle *File) {
	handle.store = store
	store.active[handle.ObjectID]++
}

func (store *Store) releaseHandle(objectID int64) {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	if store.active[objectID] <= 1 {
		delete(store.active, objectID)
		return
	}
	store.active[objectID]--
}

func (store *Store) activeHandles(objectID int64) int {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	return store.active[objectID]
}
