package objectstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

func TestOpenRecoversCrashStateMatrixWithoutReplacingMissingObjects(t *testing.T) {
	root := t.TempDir()
	opened := openTestDatabase(t, root)
	storePath := filepath.Join(root, "objects")
	initial, err := Open(storePath, opened)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	if err := initial.Close(); err != nil {
		t.Fatalf("initial Close: %v", err)
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	stagingMissing := insertObjectRecord(t, opened, stateStaging, now.Add(-6*time.Hour), nil)
	stagingFile := insertObjectRecord(t, opened, stateStaging, now.Add(-5*time.Hour), nil)
	stagingPromoted := insertObjectRecord(t, opened, stateStaging, now.Add(-4*time.Hour), nil)
	liveFile := insertObjectRecord(t, opened, stateLive, now.Add(-3*time.Hour), timePointer(now.Add(-2*time.Hour)))
	missingReferenced := insertObjectRecord(t, opened, stateStaging, now.Add(-3*time.Hour), nil)

	writeObjectFile(t, storePath, stagingDirectory, stagingFile, "staging")
	writeObjectFile(t, storePath, liveDirectory, stagingPromoted, "promoted")
	writeObjectFile(t, storePath, liveDirectory, liveFile, "live")
	repository := namespace.NewRepository(opened)
	createOverlayReference(t, repository, missingReferenced, "missing")

	recovered, err := OpenWithOptions(storePath, opened, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer recovered.Close()

	report := recovered.RecoveryReport()
	if report.RemovedRecords != 1 ||
		report.CompletedPromotions != 1 ||
		report.RetainedStaging != 1 {
		t.Errorf("recovery report = %#v", report)
	}
	if len(report.Issues) != 1 ||
		report.Issues[0].Code != IntegrityMissingReferencedObject ||
		report.Issues[0].ObjectID != missingReferenced {
		t.Fatalf("recovery issues = %#v", report.Issues)
	}
	if objectRecordExists(t, opened, stagingMissing) {
		t.Errorf("missing STAGING record %d was retained", stagingMissing)
	}
	state, _, liveAt, _ := readObject(t, opened, stagingPromoted)
	if state != stateLive || liveAt == nil || *liveAt != now.UnixNano() {
		t.Errorf("promoted object state = %q live_at=%v", state, liveAt)
	}
	state, _, liveAt, _ = readObject(t, opened, stagingFile)
	if state != stateStaging || liveAt != nil {
		t.Errorf("retained STAGING state = %q live_at=%v", state, liveAt)
	}
	if _, err := os.Stat(filepath.Join(storePath, liveDirectory, strconv.FormatInt(missingReferenced, 10))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing referenced object stat error = %v, want not exist", err)
	}
	assertObjectReferenceExists(t, opened, missingReferenced)

	gcResult, err := recovered.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if gcResult.MissingReferenced != 1 ||
		len(gcResult.Errors) != 1 ||
		gcResult.Errors[0].ObjectID != missingReferenced ||
		gcResult.Errors[0].Code != IntegrityMissingReferencedObject {
		t.Errorf("GC missing-object result = %#v", gcResult)
	}
	if gcResult.StartupRecovery.Schema != RecoveryReportSchema ||
		len(gcResult.StartupRecovery.Issues) != 1 {
		t.Errorf("GC startup recovery report = %#v", gcResult.StartupRecovery)
	}
}

func TestGarbageCollectRetainsReferencedActiveAndGracePeriodObjects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := newFixtureWithStoreOptions(t, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})

	referenced, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/referenced"),
		overlayRequest(fixture.libraryRootID, "referenced"),
		nil,
	)
	if err != nil {
		t.Fatalf("Create referenced: %v", err)
	}
	if err := referenced.Close(); err != nil {
		t.Fatalf("Close referenced: %v", err)
	}
	setObjectTimes(t, fixture.database, referenced.ObjectID, now.Add(-3*time.Hour))
	setFileTimes(t, fixture.storePath, liveDirectory, referenced.ObjectID, now.Add(-3*time.Hour))

	active, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/active"),
		overlayRequest(fixture.libraryRootID, "active"),
		nil,
	)
	if err != nil {
		t.Fatalf("Create active: %v", err)
	}
	removeOverlayReference(t, fixture.repository, fixture.libraryRootID, "active")
	setObjectTimes(t, fixture.database, active.ObjectID, now.Add(-3*time.Hour))
	setFileTimes(t, fixture.storePath, liveDirectory, active.ObjectID, now.Add(-3*time.Hour))

	recent := insertObjectRecord(
		t,
		fixture.database,
		stateLive,
		now.Add(-30*time.Minute),
		timePointer(now.Add(-30*time.Minute)),
	)
	writeObjectFile(t, fixture.storePath, liveDirectory, recent, "recent")
	setFileTimes(t, fixture.storePath, liveDirectory, recent, now.Add(-30*time.Minute))

	oldLive := insertObjectRecord(
		t,
		fixture.database,
		stateLive,
		now.Add(-3*time.Hour),
		timePointer(now.Add(-3*time.Hour)),
	)
	writeObjectFile(t, fixture.storePath, liveDirectory, oldLive, "old-live")
	setFileTimes(t, fixture.storePath, liveDirectory, oldLive, now.Add(-3*time.Hour))

	oldStaging := insertObjectRecord(t, fixture.database, stateStaging, now.Add(-3*time.Hour), nil)
	writeObjectFile(t, fixture.storePath, stagingDirectory, oldStaging, "old-staging")
	setFileTimes(t, fixture.storePath, stagingDirectory, oldStaging, now.Add(-3*time.Hour))

	result, err := fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if result.DeletedLive != 1 ||
		result.DeletedStaging != 1 ||
		result.RetainedReferenced != 1 ||
		result.RetainedActive != 1 ||
		result.RetainedGracePeriod != 1 ||
		len(result.Errors) != 0 {
		t.Errorf("GC result = %#v", result)
	}
	assertObjectAndRecordExist(t, fixture, referenced.ObjectID, true)
	assertObjectAndRecordExist(t, fixture, active.ObjectID, true)
	assertObjectAndRecordExist(t, fixture, recent, true)
	assertObjectAndRecordExist(t, fixture, oldLive, false)
	assertObjectPathExists(t, fixture.storePath, stagingDirectory, oldStaging, false)
	if objectRecordExists(t, fixture.database, oldStaging) {
		t.Errorf("STAGING object record %d still exists", oldStaging)
	}

	if err := active.Close(); err != nil {
		t.Fatalf("Close active: %v", err)
	}
	result, err = fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect after close: %v", err)
	}
	if result.DeletedLive != 1 {
		t.Errorf("GC after close result = %#v", result)
	}
	assertObjectAndRecordExist(t, fixture, active.ObjectID, false)
}

func TestGarbageCollectRechecksReferencesImmediatelyBeforeDeletion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := newFixtureWithStoreOptions(t, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})
	objectID := insertObjectRecord(
		t,
		fixture.database,
		stateStaging,
		now.Add(-3*time.Hour),
		nil,
	)
	writeObjectFile(t, fixture.storePath, stagingDirectory, objectID, "candidate")
	setFileTimes(t, fixture.storePath, stagingDirectory, objectID, now.Add(-3*time.Hour))

	fixture.store.hooks.beforeDelete = func(candidate int64) error {
		if candidate != objectID {
			return nil
		}
		createOverlayReferenceUnder(
			t,
			fixture.repository,
			fixture.libraryRootID,
			objectID,
			"rescued",
		)
		return nil
	}

	result, err := fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if result.DeletedLive != 0 || result.RetainedReferenced != 1 {
		t.Errorf("GC result = %#v", result)
	}
	assertObjectPathExists(t, fixture.storePath, stagingDirectory, objectID, true)
	if !objectRecordExists(t, fixture.database, objectID) {
		t.Fatalf("object record %d was deleted", objectID)
	}
	assertObjectReferenceExists(t, fixture.database, objectID)
}

func TestGarbageCollectReclaimsPostRenameCreateFailureWithoutRestart(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := newFixtureWithStoreOptions(t, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})

	referenced, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/referenced"),
		overlayRequest(fixture.libraryRootID, "referenced"),
		nil,
	)
	if err != nil {
		t.Fatalf("Create referenced: %v", err)
	}
	if err := referenced.Close(); err != nil {
		t.Fatalf("Close referenced: %v", err)
	}
	setObjectTimes(t, fixture.database, referenced.ObjectID, now.Add(-3*time.Hour))
	setFileTimes(t, fixture.storePath, liveDirectory, referenced.ObjectID, now.Add(-3*time.Hour))

	createFailure := errors.New("fail after rename")
	fixture.store.hooks.afterRename = func(int64) error {
		return createFailure
	}
	handle, node, err := fixture.store.CreateWithFlags(
		context.Background(),
		overlayOperation("library/abandoned"),
		overlayRequest(fixture.libraryRootID, "abandoned"),
		nil,
		0,
	)
	fixture.store.hooks.afterRename = nil
	if handle != nil {
		handle.Close()
	}
	if !errors.Is(err, createFailure) {
		t.Fatalf("CreateWithFlags error = %v, want failure after rename", err)
	}
	if node.ID != 0 {
		t.Errorf("created node = %#v, want zero value", node)
	}
	abandonedID, exists := latestObjectID(t, fixture.database)
	if !exists || abandonedID == referenced.ObjectID {
		t.Fatalf("abandoned object ID = %d exists=%v", abandonedID, exists)
	}
	state, _, liveAt, _ := readObject(t, fixture.database, abandonedID)
	if state != stateStaging || liveAt != nil {
		t.Fatalf("abandoned object state = %q live_at=%v, want STAGING/NULL", state, liveAt)
	}
	assertObjectPathExists(t, fixture.storePath, stagingDirectory, abandonedID, false)
	assertObjectPathExists(t, fixture.storePath, liveDirectory, abandonedID, true)
	setStagingObjectTimes(t, fixture.database, abandonedID, now.Add(-3*time.Hour))
	setFileTimes(t, fixture.storePath, liveDirectory, abandonedID, now.Add(-3*time.Hour))

	result, err := fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if result.DeletedStaging != 1 ||
		result.RetainedReferenced != 1 ||
		len(result.Errors) != 0 {
		t.Errorf("GC result = %#v", result)
	}
	assertObjectPathExists(t, fixture.storePath, liveDirectory, abandonedID, false)
	if objectRecordExists(t, fixture.database, abandonedID) {
		t.Errorf("abandoned STAGING record %d still exists", abandonedID)
	}
	assertObjectAndRecordExist(t, fixture, referenced.ObjectID, true)
	assertObjectReferenceExists(t, fixture.database, referenced.ObjectID)
}

func TestGarbageCollectRetainsUntrackedObjectWithActiveHandle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := newFixtureWithStoreOptions(t, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})
	handle, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/untracked-active"),
		overlayRequest(fixture.libraryRootID, "untracked-active"),
		nil,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	objectID := handle.ObjectID
	removeOverlayReference(
		t,
		fixture.repository,
		fixture.libraryRootID,
		"untracked-active",
	)
	deleteObjectRecord(t, fixture.database, objectID)
	setFileTimes(t, fixture.storePath, liveDirectory, objectID, now.Add(-3*time.Hour))

	result, err := fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if result.DeletedUntracked != 0 || result.RetainedActive != 1 {
		t.Errorf("GC result = %#v", result)
	}
	assertObjectPathExists(t, fixture.storePath, liveDirectory, objectID, true)
	var stat unix.Stat_t
	if err := unix.Fstat(int(handle.Fd()), &stat); err != nil {
		t.Fatalf("Fstat active handle: %v", err)
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	result, err = fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect after close: %v", err)
	}
	if result.DeletedUntracked != 1 || result.RetainedActive != 0 {
		t.Errorf("GC after close result = %#v", result)
	}
	assertObjectPathExists(t, fixture.storePath, liveDirectory, objectID, false)
}

func TestGarbageCollectRechecksUntrackedActiveHandleBeforeDeletion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := newFixtureWithStoreOptions(t, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})
	const objectID int64 = 99
	writeObjectFile(t, fixture.storePath, liveDirectory, objectID, "untracked")
	setFileTimes(t, fixture.storePath, liveDirectory, objectID, now.Add(-3*time.Hour))

	var handle *File
	fixture.store.hooks.beforeDelete = func(candidate int64) error {
		if candidate != objectID {
			return nil
		}
		raw, err := openExistingObject(fixture.store.live, objectID, unix.O_RDWR)
		if err != nil {
			return err
		}
		handle = &File{File: raw, ObjectID: objectID}
		fixture.store.trackHandle(handle)
		return nil
	}

	result, err := fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if result.DeletedUntracked != 0 || result.RetainedActive != 1 {
		t.Errorf("GC result = %#v", result)
	}
	assertObjectPathExists(t, fixture.storePath, liveDirectory, objectID, true)
	if handle == nil {
		t.Fatal("before-delete hook did not open a handle")
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("Close injected handle: %v", err)
	}

	fixture.store.hooks.beforeDelete = nil
	result, err = fixture.store.GarbageCollect(context.Background())
	if err != nil {
		t.Fatalf("GarbageCollect after close: %v", err)
	}
	if result.DeletedUntracked != 1 {
		t.Errorf("GC after close result = %#v", result)
	}
	assertObjectPathExists(t, fixture.storePath, liveDirectory, objectID, false)
}

func TestDeleteObjectDefersReclamationWhileHandleIsActive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := newFixtureWithStoreOptions(t, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})
	handle, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/poster"),
		overlayRequest(fixture.libraryRootID, "poster"),
		nil,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	removeOverlayReference(t, fixture.repository, fixture.libraryRootID, "poster")

	if err := fixture.store.DeleteObject(handle.ObjectID); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	assertObjectAndRecordExist(t, fixture, handle.ObjectID, true)
	if _, err := handle.WriteAt([]byte("still open"), 0); err != nil {
		t.Fatalf("WriteAt active handle: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func insertObjectRecord(
	t *testing.T,
	opened *database.DB,
	state string,
	createdAt time.Time,
	liveAt *time.Time,
) int64 {
	t.Helper()
	var objectID int64
	var liveAtNS any
	if liveAt != nil {
		liveAtNS = liveAt.UnixNano()
	}
	updatedAt := createdAt.UnixNano()
	if liveAt != nil && liveAt.UnixNano() > updatedAt {
		updatedAt = liveAt.UnixNano()
	}
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		result, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer result.Rollback()
		inserted, err := result.ExecContext(context.Background(), `
			INSERT INTO objects (state, created_at_ns, live_at_ns, updated_at_ns)
			VALUES (?, ?, ?, ?)
		`, state, createdAt.UnixNano(), liveAtNS, updatedAt)
		if err != nil {
			return err
		}
		objectID, err = inserted.LastInsertId()
		if err != nil {
			return err
		}
		return result.Commit()
	}); err != nil {
		t.Fatalf("insert object record: %v", err)
	}
	return objectID
}

func writeObjectFile(t *testing.T, storePath, directory string, objectID int64, content string) {
	t.Helper()
	path := filepath.Join(storePath, directory, strconv.FormatInt(objectID, 10))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod(%q): %v", path, err)
	}
}

func setObjectTimes(t *testing.T, opened *database.DB, objectID int64, at time.Time) {
	t.Helper()
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(context.Background(), `
			UPDATE objects
			SET created_at_ns = ?, live_at_ns = ?, updated_at_ns = ?
			WHERE id = ?
		`, at.UnixNano(), at.UnixNano(), at.UnixNano(), objectID); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("set object times: %v", err)
	}
}

func setStagingObjectTimes(t *testing.T, opened *database.DB, objectID int64, at time.Time) {
	t.Helper()
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(context.Background(), `
			UPDATE objects
			SET created_at_ns = ?, updated_at_ns = ?
			WHERE id = ? AND state = 'STAGING'
		`, at.UnixNano(), at.UnixNano(), objectID); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("set STAGING object times: %v", err)
	}
}

func deleteObjectRecord(t *testing.T, opened *database.DB, objectID int64) {
	t.Helper()
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(
			context.Background(),
			"DELETE FROM objects WHERE id = ?",
			objectID,
		); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("delete object record: %v", err)
	}
}

func setFileTimes(t *testing.T, storePath, directory string, objectID int64, at time.Time) {
	t.Helper()
	path := filepath.Join(storePath, directory, strconv.FormatInt(objectID, 10))
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("Chtimes(%q): %v", path, err)
	}
}

func createOverlayReference(t *testing.T, repository *namespace.Repository, objectID int64, name string) {
	t.Helper()
	rootID := createLibraryRoot(t, repository)
	createOverlayReferenceUnder(t, repository, rootID, objectID, name)
}

func createOverlayReferenceUnder(
	t *testing.T,
	repository *namespace.Repository,
	parentID int64,
	objectID int64,
	name string,
) {
	t.Helper()
	if _, err := repository.Update(
		context.Background(),
		overlayOperation("library/"+name),
		func(transaction *namespace.Transaction) error {
			_, err := transaction.CreateOverlay(
				context.Background(),
				objectID,
				overlayRequest(parentID, name),
			)
			return err
		},
	); err != nil {
		t.Fatalf("create Overlay reference: %v", err)
	}
}

func removeOverlayReference(
	t *testing.T,
	repository *namespace.Repository,
	parentID int64,
	name string,
) {
	t.Helper()
	if _, err := repository.Update(
		context.Background(),
		overlayOperation("library/"+name),
		func(transaction *namespace.Transaction) error {
			_, err := transaction.RemoveOverlay(context.Background(), parentID, name)
			return err
		},
	); err != nil {
		t.Fatalf("remove Overlay reference: %v", err)
	}
}

func assertObjectReferenceExists(t *testing.T, opened *database.DB, objectID int64) {
	t.Helper()
	var count int
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM nodes WHERE object_id = ?",
			objectID,
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count object references: %v", err)
	}
	if count == 0 {
		t.Fatalf("object %d has no namespace reference", objectID)
	}
}

func objectRecordExists(t *testing.T, opened *database.DB, objectID int64) bool {
	t.Helper()
	var count int
	if err := opened.UseConnection(context.Background(), func(connection *database.Connection) error {
		return connection.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM objects WHERE id = ?",
			objectID,
		).Scan(&count)
	}); err != nil {
		t.Fatalf("count object record: %v", err)
	}
	return count == 1
}

func assertObjectAndRecordExist(t *testing.T, fixture fixture, objectID int64, want bool) {
	t.Helper()
	assertObjectPathExists(t, fixture.storePath, liveDirectory, objectID, want)
	if got := objectRecordExists(t, fixture.database, objectID); got != want {
		t.Errorf("object record %d exists = %v, want %v", objectID, got, want)
	}
}

func assertObjectPathExists(
	t *testing.T,
	storePath string,
	directory string,
	objectID int64,
	want bool,
) {
	t.Helper()
	_, err := os.Stat(filepath.Join(storePath, directory, strconv.FormatInt(objectID, 10)))
	if want && err != nil {
		t.Errorf("object %d stat: %v", objectID, err)
	}
	if !want && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("object %d stat error = %v, want not exist", objectID, err)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func newFixtureWithStoreOptions(t *testing.T, options Options) fixture {
	t.Helper()
	root := t.TempDir()
	opened := openTestDatabase(t, root)
	storePath := filepath.Join(root, "objects")
	store, err := OpenWithOptions(storePath, opened, options)
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close object store: %v", err)
		}
	})
	repository := namespace.NewRepository(opened)
	return fixture{
		root:          root,
		storePath:     storePath,
		database:      opened,
		store:         store,
		repository:    repository,
		libraryRootID: createLibraryRoot(t, repository),
	}
}

func TestGarbageCollectSerializesWithConcurrentObjectCreation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	fixture := newFixtureWithStoreOptions(t, Options{
		OrphanGracePeriod: time.Hour,
		Now:               func() time.Time { return now },
	})
	objectID := insertObjectRecord(
		t,
		fixture.database,
		stateLive,
		now.Add(-3*time.Hour),
		timePointer(now.Add(-3*time.Hour)),
	)
	writeObjectFile(t, fixture.storePath, liveDirectory, objectID, "candidate")
	setFileTimes(t, fixture.storePath, liveDirectory, objectID, now.Add(-3*time.Hour))

	gcEntered := make(chan struct{})
	releaseGC := make(chan struct{})
	fixture.store.hooks.beforeDelete = func(candidate int64) error {
		if candidate == objectID {
			close(gcEntered)
			<-releaseGC
		}
		return nil
	}
	gcDone := make(chan error, 1)
	go func() {
		_, err := fixture.store.GarbageCollect(context.Background())
		gcDone <- err
	}()
	<-gcEntered

	createDone := make(chan error, 1)
	go func() {
		handle, _, err := fixture.store.Create(
			context.Background(),
			overlayOperation("library/concurrent"),
			overlayRequest(fixture.libraryRootID, "concurrent"),
			nil,
		)
		if handle != nil {
			err = errors.Join(err, handle.Close())
		}
		createDone <- err
	}()
	select {
	case err := <-createDone:
		t.Fatalf("concurrent Create completed before GC released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseGC)
	if err := <-gcDone; err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if err := <-createDone; err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestOpenObjectTracksActiveHandlesUntilClose(t *testing.T) {
	fixture := newFixture(t)
	handle, _, err := fixture.store.Create(
		context.Background(),
		overlayOperation("library/tracked"),
		overlayRequest(fixture.libraryRootID, "tracked"),
		nil,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	opened, err := fixture.store.OpenObject(context.Background(), handle.ObjectID, unix.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	if got := fixture.store.activeHandles(handle.ObjectID); got != 2 {
		t.Fatalf("active handles = %d, want 2", got)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close opened: %v", err)
	}
	if got := fixture.store.activeHandles(handle.ObjectID); got != 1 {
		t.Fatalf("active handles after first close = %d, want 1", got)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("Close created: %v", err)
	}
	if got := fixture.store.activeHandles(handle.ObjectID); got != 0 {
		t.Fatalf("active handles after closes = %d, want 0", got)
	}
}
