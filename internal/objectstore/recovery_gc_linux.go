package objectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/zendext/scrape-fs/internal/database"
	"golang.org/x/sys/unix"
)

const (
	RecoveryReportSchema = "scrapefs.object-recovery/v1"
	GCResultSchema       = "scrapefs.gc-result/v1"

	IntegrityMissingReferencedObject = "MissingReferencedObject"
	IntegrityReferencedStagingObject = "ReferencedStagingObject"
	IntegrityConflictingObjectFiles  = "ConflictingObjectFiles"
	IntegrityUnexpectedObjectFile    = "UnexpectedObjectFile"
)

type IntegrityIssue struct {
	ObjectID int64   `json:"object_id"`
	Code     string  `json:"code"`
	NodeIDs  []int64 `json:"node_ids,omitempty"`
	Message  string  `json:"message"`
}

type RecoveryReport struct {
	Schema              string           `json:"schema"`
	ScannedRecords      int              `json:"scanned_records"`
	RemovedRecords      int              `json:"removed_records"`
	CompletedPromotions int              `json:"completed_promotions"`
	RetainedStaging     int              `json:"retained_staging"`
	Issues              []IntegrityIssue `json:"issues"`
}

type GCError struct {
	ObjectID int64  `json:"object_id"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type GCResult struct {
	Schema              string         `json:"schema"`
	StartupRecovery     RecoveryReport `json:"startup_recovery"`
	Examined            int            `json:"examined"`
	DeletedLive         int            `json:"deleted_live"`
	DeletedStaging      int            `json:"deleted_staging"`
	DeletedUntracked    int            `json:"deleted_untracked"`
	RetainedReferenced  int            `json:"retained_referenced"`
	RetainedActive      int            `json:"retained_active"`
	RetainedGracePeriod int            `json:"retained_grace_period"`
	MissingReferenced   int            `json:"missing_referenced"`
	Errors              []GCError      `json:"errors"`
}

type objectRecord struct {
	id          int64
	state       string
	createdAtNS int64
	liveAtNS    *int64
	updatedAtNS int64
}

type objectEntry struct {
	mtimeNS int64
}

type reclaimOutcome uint8

const (
	reclaimNone reclaimOutcome = iota
	reclaimDeletedLive
	reclaimDeletedStaging
	reclaimDeletedUntracked
	reclaimRetainedReferenced
	reclaimRetainedActive
	reclaimRetainedGrace
)

func (store *Store) RecoveryReport() RecoveryReport {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	return cloneRecoveryReport(store.recovery)
}

func cloneRecoveryReport(report RecoveryReport) RecoveryReport {
	report.Issues = append([]IntegrityIssue(nil), report.Issues...)
	for index := range report.Issues {
		report.Issues[index].NodeIDs = append([]int64(nil), report.Issues[index].NodeIDs...)
	}
	return report
}

func (store *Store) recover(ctx context.Context) (RecoveryReport, error) {
	report := RecoveryReport{
		Schema: RecoveryReportSchema,
		Issues: []IntegrityIssue{},
	}
	staging, err := listObjectEntries(store.staging)
	if err != nil {
		return report, err
	}
	live, err := listObjectEntries(store.live)
	if err != nil {
		return report, err
	}

	err = store.database.UseConnection(ctx, func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin object-store recovery transaction: %w", err)
		}
		defer transaction.Rollback()

		records, err := readObjectRecords(ctx, transaction)
		if err != nil {
			return err
		}
		report.ScannedRecords = len(records)
		known := make(map[int64]struct{}, len(records))
		now := store.now().UTC().UnixNano()
		for _, record := range records {
			known[record.id] = struct{}{}
			nodeIDs, err := referencedNodeIDs(ctx, transaction, record.id)
			if err != nil {
				return err
			}
			_, inStaging := staging[record.id]
			_, inLive := live[record.id]
			if inStaging && inLive {
				report.Issues = append(report.Issues, IntegrityIssue{
					ObjectID: record.id,
					Code:     IntegrityConflictingObjectFiles,
					NodeIDs:  nodeIDs,
					Message:  fmt.Sprintf("object %d exists in both staging and live", record.id),
				})
				continue
			}

			switch record.state {
			case stateStaging:
				if len(nodeIDs) > 0 {
					report.Issues = append(report.Issues, IntegrityIssue{
						ObjectID: record.id,
						Code:     IntegrityReferencedStagingObject,
						NodeIDs:  nodeIDs,
						Message:  fmt.Sprintf("STAGING object %d is referenced by namespace nodes", record.id),
					})
					continue
				}
				switch {
				case inLive:
					liveAt := now
					if liveAt < record.createdAtNS {
						liveAt = record.createdAtNS
					}
					if _, err := transaction.ExecContext(ctx, `
						UPDATE objects
						SET state = 'LIVE', live_at_ns = ?, updated_at_ns = ?
						WHERE id = ? AND state = 'STAGING'
					`, liveAt, liveAt, record.id); err != nil {
						return fmt.Errorf("complete object %d promotion: %w", record.id, err)
					}
					report.CompletedPromotions++
				case inStaging:
					report.RetainedStaging++
				default:
					if _, err := transaction.ExecContext(
						ctx,
						"DELETE FROM objects WHERE id = ? AND state = 'STAGING'",
						record.id,
					); err != nil {
						return fmt.Errorf("remove missing STAGING object %d: %w", record.id, err)
					}
					report.RemovedRecords++
				}
			case stateLive:
				if !inLive && len(nodeIDs) > 0 {
					report.Issues = append(report.Issues, IntegrityIssue{
						ObjectID: record.id,
						Code:     IntegrityMissingReferencedObject,
						NodeIDs:  nodeIDs,
						Message:  fmt.Sprintf("referenced LIVE object %d is missing", record.id),
					})
				}
			default:
				return fmt.Errorf("object %d has invalid state %q", record.id, record.state)
			}
		}
		for objectID := range staging {
			if _, exists := known[objectID]; !exists {
				if _, duplicate := live[objectID]; duplicate {
					report.Issues = append(report.Issues, IntegrityIssue{
						ObjectID: objectID,
						Code:     IntegrityConflictingObjectFiles,
						Message:  fmt.Sprintf("untracked object %d exists in both staging and live", objectID),
					})
					continue
				}
				report.Issues = append(report.Issues, IntegrityIssue{
					ObjectID: objectID,
					Code:     IntegrityUnexpectedObjectFile,
					Message:  fmt.Sprintf("staging object %d has no database record", objectID),
				})
			}
		}
		for objectID := range live {
			if _, exists := known[objectID]; !exists {
				if _, duplicate := staging[objectID]; duplicate {
					continue
				}
				report.Issues = append(report.Issues, IntegrityIssue{
					ObjectID: objectID,
					Code:     IntegrityUnexpectedObjectFile,
					Message:  fmt.Sprintf("live object %d has no database record", objectID),
				})
			}
		}
		sortIntegrityIssues(report.Issues)
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit object-store recovery: %w", err)
		}
		return nil
	})
	return report, err
}

func (store *Store) GarbageCollect(ctx context.Context) (GCResult, error) {
	store.coordinator.Lock()
	defer store.coordinator.Unlock()
	result := GCResult{
		Schema:          GCResultSchema,
		StartupRecovery: cloneRecoveryReport(store.recovery),
		Errors:          []GCError{},
	}
	if store.closed {
		return result, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	records, err := store.objectRecords(ctx)
	if err != nil {
		return result, err
	}
	staging, err := listObjectEntries(store.staging)
	if err != nil {
		return result, err
	}
	live, err := listObjectEntries(store.live)
	if err != nil {
		return result, err
	}
	known := make(map[int64]struct{}, len(records))
	for _, record := range records {
		known[record.id] = struct{}{}
		result.Examined++
		references, err := store.objectReferences(ctx, record.id)
		if err != nil {
			return result, err
		}
		_, inStaging := staging[record.id]
		_, inLive := live[record.id]
		if references > 0 {
			result.RetainedReferenced++
			if record.state == stateLive && !inLive {
				result.MissingReferenced++
				result.Errors = append(result.Errors, GCError{
					ObjectID: record.id,
					Code:     IntegrityMissingReferencedObject,
					Message:  fmt.Sprintf("referenced LIVE object %d is missing", record.id),
				})
			} else if record.state == stateStaging {
				result.Errors = append(result.Errors, GCError{
					ObjectID: record.id,
					Code:     IntegrityReferencedStagingObject,
					Message:  fmt.Sprintf("STAGING object %d is referenced by namespace nodes", record.id),
				})
			}
			continue
		}
		if store.active[record.id] > 0 {
			result.RetainedActive++
			continue
		}
		if inStaging && inLive {
			result.Errors = append(result.Errors, GCError{
				ObjectID: record.id,
				Code:     IntegrityConflictingObjectFiles,
				Message:  fmt.Sprintf("object %d exists in both staging and live", record.id),
			})
			continue
		}
		if !recordEligible(record, staging[record.id], live[record.id], store.cutoff()) {
			result.RetainedGracePeriod++
			continue
		}
		if err := runHook(store.hooks.beforeDelete, record.id); err != nil {
			result.Errors = append(result.Errors, GCError{
				ObjectID: record.id,
				Code:     "DeleteHookFailed",
				Message:  err.Error(),
			})
			continue
		}
		outcome, err := store.reclaimRecord(ctx, record.id, true)
		if err != nil {
			result.Errors = append(result.Errors, GCError{
				ObjectID: record.id,
				Code:     "DeleteFailed",
				Message:  err.Error(),
			})
			continue
		}
		applyReclaimOutcome(&result, outcome)
	}

	orphanIDs := make([]int64, 0)
	for objectID := range staging {
		if _, exists := known[objectID]; !exists {
			orphanIDs = append(orphanIDs, objectID)
		}
	}
	for objectID := range live {
		if _, exists := known[objectID]; !exists {
			orphanIDs = append(orphanIDs, objectID)
		}
	}
	sort.Slice(orphanIDs, func(left, right int) bool {
		return orphanIDs[left] < orphanIDs[right]
	})
	orphanIDs = compactObjectIDs(orphanIDs)
	for _, objectID := range orphanIDs {
		result.Examined++
		if store.active[objectID] > 0 {
			result.RetainedActive++
			continue
		}
		stagingEntry, inStaging := staging[objectID]
		liveEntry, inLive := live[objectID]
		if inStaging && inLive {
			result.Errors = append(result.Errors, GCError{
				ObjectID: objectID,
				Code:     IntegrityConflictingObjectFiles,
				Message:  fmt.Sprintf("untracked object %d exists in both staging and live", objectID),
			})
			continue
		}
		entry := stagingEntry
		directory := store.staging
		if inLive {
			entry = liveEntry
			directory = store.live
		}
		if newestFileTimestamp(entry) > store.cutoff() {
			result.RetainedGracePeriod++
			continue
		}
		if err := runHook(store.hooks.beforeDelete, objectID); err != nil {
			result.Errors = append(result.Errors, GCError{
				ObjectID: objectID,
				Code:     "DeleteHookFailed",
				Message:  err.Error(),
			})
			continue
		}
		outcome, err := store.reclaimUntracked(ctx, directory, objectID)
		if err != nil {
			result.Errors = append(result.Errors, GCError{
				ObjectID: objectID,
				Code:     "DeleteFailed",
				Message:  err.Error(),
			})
			continue
		}
		applyReclaimOutcome(&result, outcome)
	}
	sort.Slice(result.Errors, func(left, right int) bool {
		if result.Errors[left].ObjectID != result.Errors[right].ObjectID {
			return result.Errors[left].ObjectID < result.Errors[right].ObjectID
		}
		if result.Errors[left].Code != result.Errors[right].Code {
			return result.Errors[left].Code < result.Errors[right].Code
		}
		return result.Errors[left].Message < result.Errors[right].Message
	})
	return result, nil
}

func (store *Store) reclaimRecord(
	ctx context.Context,
	objectID int64,
	enforceGrace bool,
) (reclaimOutcome, error) {
	if store.active[objectID] > 0 {
		return reclaimRetainedActive, nil
	}
	outcome := reclaimNone
	err := store.database.UseConnection(ctx, func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin object %d reclamation: %w", objectID, err)
		}
		defer transaction.Rollback()
		record, err := readObjectRecord(ctx, transaction, objectID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		references, err := referenceCount(ctx, transaction, objectID)
		if err != nil {
			return err
		}
		if references > 0 {
			outcome = reclaimRetainedReferenced
			return nil
		}
		if store.active[objectID] > 0 {
			outcome = reclaimRetainedActive
			return nil
		}
		stagingEntry, inStaging, err := statObjectEntry(store.staging, objectID)
		if err != nil {
			return err
		}
		liveEntry, inLive, err := statObjectEntry(store.live, objectID)
		if err != nil {
			return err
		}
		if inStaging && inLive {
			return fmt.Errorf("object %d exists in both staging and live", objectID)
		}
		if enforceGrace && !recordEligible(record, stagingEntry, liveEntry, store.cutoff()) {
			outcome = reclaimRetainedGrace
			return nil
		}

		var directory *os.File
		switch record.state {
		case stateStaging:
			if inStaging {
				directory = store.staging
			} else if inLive {
				directory = store.live
			}
			outcome = reclaimDeletedStaging
		case stateLive:
			if inStaging {
				return fmt.Errorf("LIVE object %d unexpectedly exists in staging", objectID)
			}
			if inLive {
				directory = store.live
			}
			outcome = reclaimDeletedLive
		default:
			return fmt.Errorf("object %d has invalid state %q", objectID, record.state)
		}
		if directory != nil {
			if err := unlinkObject(directory, objectID); err != nil {
				return err
			}
		}
		if _, err := transaction.ExecContext(
			ctx,
			"DELETE FROM objects WHERE id = ?",
			objectID,
		); err != nil {
			return fmt.Errorf("delete object %d record: %w", objectID, err)
		}
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit object %d reclamation: %w", objectID, err)
		}
		return nil
	})
	return outcome, err
}

func (store *Store) reclaimUntracked(
	ctx context.Context,
	directory *os.File,
	objectID int64,
) (reclaimOutcome, error) {
	if store.active[objectID] > 0 {
		return reclaimRetainedActive, nil
	}
	outcome := reclaimNone
	err := store.database.UseConnection(ctx, func(connection *database.Connection) error {
		transaction, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin untracked object %d reclamation: %w", objectID, err)
		}
		defer transaction.Rollback()
		if _, err := readObjectRecord(ctx, transaction, objectID); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if store.active[objectID] > 0 {
			outcome = reclaimRetainedActive
			return nil
		}
		entry, exists, err := statObjectEntry(directory, objectID)
		if err != nil {
			return err
		}
		if !exists || newestFileTimestamp(entry) > store.cutoff() {
			return nil
		}
		if err := unlinkObject(directory, objectID); err != nil {
			return err
		}
		outcome = reclaimDeletedUntracked
		return transaction.Commit()
	})
	return outcome, err
}

func (store *Store) objectRecords(ctx context.Context) ([]objectRecord, error) {
	var records []objectRecord
	err := store.database.UseConnection(ctx, func(connection *database.Connection) error {
		var err error
		records, err = readObjectRecords(ctx, connection)
		return err
	})
	return records, err
}

func (store *Store) objectReferences(ctx context.Context, objectID int64) (int, error) {
	var count int
	err := store.database.UseConnection(ctx, func(connection *database.Connection) error {
		var err error
		count, err = referenceCount(ctx, connection, objectID)
		return err
	})
	return count, err
}

func (store *Store) cutoff() int64 {
	return store.now().UTC().Add(-store.gracePeriod).UnixNano()
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *database.Row
}

func readObjectRecords(ctx context.Context, query rowQuerier) ([]objectRecord, error) {
	records := []objectRecord{}
	var cursor int64
	for {
		record, err := readNextObjectRecord(ctx, query, cursor)
		if errors.Is(err, sql.ErrNoRows) {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		cursor = record.id
	}
}

func readNextObjectRecord(
	ctx context.Context,
	query rowQuerier,
	cursor int64,
) (objectRecord, error) {
	var record objectRecord
	err := query.QueryRowContext(ctx, `
		SELECT id, state, created_at_ns, live_at_ns, updated_at_ns
		FROM objects
		WHERE id > ?
		ORDER BY id
		LIMIT 1
	`, cursor).Scan(
		&record.id,
		&record.state,
		&record.createdAtNS,
		&record.liveAtNS,
		&record.updatedAtNS,
	)
	if err != nil {
		return objectRecord{}, err
	}
	return record, nil
}

func readObjectRecord(
	ctx context.Context,
	query rowQuerier,
	objectID int64,
) (objectRecord, error) {
	var record objectRecord
	err := query.QueryRowContext(ctx, `
		SELECT id, state, created_at_ns, live_at_ns, updated_at_ns
		FROM objects
		WHERE id = ?
	`, objectID).Scan(
		&record.id,
		&record.state,
		&record.createdAtNS,
		&record.liveAtNS,
		&record.updatedAtNS,
	)
	return record, err
}

func referenceCount(ctx context.Context, query rowQuerier, objectID int64) (int, error) {
	var count int
	if err := query.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM nodes WHERE object_id = ?",
		objectID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count object %d references: %w", objectID, err)
	}
	return count, nil
}

func referencedNodeIDs(ctx context.Context, query rowQuerier, objectID int64) ([]int64, error) {
	nodeIDs := []int64{}
	var cursor int64
	for {
		var nodeID int64
		err := query.QueryRowContext(ctx, `
			SELECT id
			FROM nodes
			WHERE object_id = ? AND id > ?
			ORDER BY id
			LIMIT 1
		`, objectID, cursor).Scan(&nodeID)
		if errors.Is(err, sql.ErrNoRows) {
			return nodeIDs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list object %d references: %w", objectID, err)
		}
		nodeIDs = append(nodeIDs, nodeID)
		cursor = nodeID
	}
}

func listObjectEntries(directory *os.File) (map[int64]objectEntry, error) {
	if _, err := directory.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind object-store directory: %w", err)
	}
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list object-store directory: %w", err)
	}
	entries := make(map[int64]objectEntry, len(names))
	for _, name := range names {
		objectID, err := strconv.ParseInt(name, 10, 64)
		if err != nil || objectID <= 0 || strconv.FormatInt(objectID, 10) != name {
			return nil, fmt.Errorf("object-store entry %q is not a canonical object ID", name)
		}
		entry, exists, err := statObjectEntry(directory, objectID)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		entries[objectID] = entry
	}
	return entries, nil
}

func statObjectEntry(
	directory *os.File,
	objectID int64,
) (objectEntry, bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(
		int(directory.Fd()),
		strconv.FormatInt(objectID, 10),
		&stat,
		unix.AT_SYMLINK_NOFOLLOW,
	)
	if errors.Is(err, unix.ENOENT) {
		return objectEntry{}, false, nil
	}
	if err != nil {
		return objectEntry{}, false, fmt.Errorf("stat object %d: %w", objectID, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return objectEntry{}, false, fmt.Errorf("object %d is not a regular file", objectID)
	}
	return objectEntry{
		mtimeNS: unix.TimespecToNsec(stat.Mtim),
	}, true, nil
}

func recordEligible(
	record objectRecord,
	staging objectEntry,
	live objectEntry,
	cutoff int64,
) bool {
	timestamp := record.updatedAtNS
	if record.liveAtNS != nil && *record.liveAtNS > timestamp {
		timestamp = *record.liveAtNS
	}
	if record.createdAtNS > timestamp {
		timestamp = record.createdAtNS
	}
	for _, entry := range []objectEntry{staging, live} {
		if fileTimestamp := newestFileTimestamp(entry); fileTimestamp > timestamp {
			timestamp = fileTimestamp
		}
	}
	return timestamp <= cutoff
}

func newestFileTimestamp(entry objectEntry) int64 {
	return entry.mtimeNS
}

func unlinkObject(directory *os.File, objectID int64) error {
	if err := unix.Unlinkat(
		int(directory.Fd()),
		strconv.FormatInt(objectID, 10),
		0,
	); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("delete object %d: %w", objectID, err)
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("fsync object directory after deleting object %d: %w", objectID, err)
	}
	return nil
}

func applyReclaimOutcome(result *GCResult, outcome reclaimOutcome) {
	switch outcome {
	case reclaimDeletedLive:
		result.DeletedLive++
	case reclaimDeletedStaging:
		result.DeletedStaging++
	case reclaimDeletedUntracked:
		result.DeletedUntracked++
	case reclaimRetainedReferenced:
		result.RetainedReferenced++
	case reclaimRetainedActive:
		result.RetainedActive++
	case reclaimRetainedGrace:
		result.RetainedGracePeriod++
	}
}

func compactObjectIDs(ids []int64) []int64 {
	if len(ids) < 2 {
		return ids
	}
	compacted := ids[:1]
	for _, id := range ids[1:] {
		if id != compacted[len(compacted)-1] {
			compacted = append(compacted, id)
		}
	}
	return compacted
}

func sortIntegrityIssues(issues []IntegrityIssue) {
	sort.Slice(issues, func(left, right int) bool {
		if issues[left].ObjectID != issues[right].ObjectID {
			return issues[left].ObjectID < issues[right].ObjectID
		}
		if issues[left].Code != issues[right].Code {
			return issues[left].Code < issues[right].Code
		}
		return issues[left].Message < issues[right].Message
	})
}
