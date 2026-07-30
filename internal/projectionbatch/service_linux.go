package projectionbatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
)

var (
	ErrDigestMismatch = errors.New("Projection Batch digest mismatch")
	ErrBatchNotFound  = errors.New("Projection Batch not found")
	ErrAlreadyUndone  = errors.New("Projection Batch already undone")
)

type Caller struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type ApplyRequest struct {
	Data           []byte
	ExpectedDigest string
	Actor          namespace.Actor
	Caller         Caller
}

type UndoRequest struct {
	BatchID string
	Actor   namespace.Actor
	Caller  Caller
}

type ValidationResult struct {
	Schema           string            `json:"schema"`
	Digest           string            `json:"digest"`
	NamespaceVersion int64             `json:"namespace_version"`
	Valid            bool              `json:"valid"`
	Entries          []ValidationEntry `json:"entries"`
	Warnings         []string          `json:"warnings"`
}

type ValidationEntry struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Action string `json:"action"`
}

type ApplyResult struct {
	Schema           string `json:"schema"`
	BatchID          string `json:"batch_id"`
	Digest           string `json:"digest"`
	NamespaceVersion int64  `json:"namespace_version"`
}

type ListResult struct {
	Schema  string         `json:"schema"`
	Batches []BatchSummary `json:"batches"`
}

type DetailResult struct {
	Schema     string              `json:"schema"`
	Batch      BatchSummary        `json:"batch"`
	Entries    []DetailEntry       `json:"entries"`
	Operations []OperationSnapshot `json:"operations"`
}

type UndoResult struct {
	Schema               string   `json:"schema"`
	BatchID              string   `json:"batch_id"`
	Status               string   `json:"status"`
	NamespaceVersion     int64    `json:"namespace_version"`
	RetainedOverlayFiles []string `json:"retained_overlay_files"`
}

type BatchSummary struct {
	BatchID     string  `json:"batch_id"`
	Library     string  `json:"library"`
	Profile     string  `json:"profile"`
	Digest      string  `json:"digest"`
	Status      string  `json:"status"`
	AppliedBy   Caller  `json:"applied_by"`
	AppliedAtNS int64   `json:"applied_at_ns"`
	UndoneBy    *Caller `json:"undone_by,omitempty"`
	UndoneAtNS  *int64  `json:"undone_at_ns,omitempty"`
}

type DetailEntry struct {
	Position         int64          `json:"position"`
	Source           SourceIdentity `json:"source"`
	Target           string         `json:"target"`
	ProjectionNodeID *int64         `json:"projection_node_id"`
}

type SourceIdentity struct {
	Path          string `json:"path"`
	DeviceID      int64  `json:"device_id"`
	Inode         int64  `json:"inode"`
	Size          int64  `json:"size"`
	AtimeNS       int64  `json:"atime_ns"`
	MtimeNS       int64  `json:"mtime_ns"`
	SourceCtimeNS int64  `json:"source_ctime_ns"`
	VerifiedAtNS  int64  `json:"verified_at_ns"`
}

type OperationSnapshot struct {
	Name             string                     `json:"name"`
	Actor            ActorSnapshot              `json:"actor"`
	NamespaceVersion int64                      `json:"namespace_version"`
	Details          namespace.OperationDetails `json:"details"`
	CreatedAtNS      int64                      `json:"created_at_ns"`
}

type ActorSnapshot struct {
	Kind string `json:"kind"`
	ID   *int64 `json:"id,omitempty"`
}

type Service struct {
	repository *namespace.Repository
	projection *projection.Service
	now        func() time.Time
}

func NewService(repository *namespace.Repository) *Service {
	return &Service{
		repository: repository,
		projection: projection.NewService(repository),
		now:        time.Now,
	}
}

func (service *Service) Validate(
	ctx context.Context,
	data []byte,
) (ValidationResult, error) {
	parsed, candidates, version, err := service.prepare(ctx, data)
	if err != nil {
		return ValidationResult{}, err
	}

	entries := make([]ValidationEntry, len(candidates))
	for index, candidate := range candidates {
		entries[index] = ValidationEntry{
			Source: candidate.Source.Path,
			Target: candidate.Target,
			Action: "create",
		}
	}
	return ValidationResult{
		Schema:           ValidationSchema,
		Digest:           parsed.Digest,
		NamespaceVersion: version,
		Valid:            true,
		Entries:          entries,
		Warnings:         []string{},
	}, nil
}

func (service *Service) Apply(
	ctx context.Context,
	request ApplyRequest,
) (ApplyResult, error) {
	parsed, err := Parse(request.Data)
	if err != nil {
		return ApplyResult{}, err
	}
	if parsed.Digest != request.ExpectedDigest {
		return ApplyResult{}, fmt.Errorf(
			"%w: expected %q, got %q",
			ErrDigestMismatch,
			request.ExpectedDigest,
			parsed.Digest,
		)
	}
	_, candidates, _, err := service.prepareParsed(ctx, parsed)
	if err != nil {
		return ApplyResult{}, err
	}
	return service.applyPrepared(ctx, request, parsed, candidates)
}

func (service *Service) applyPrepared(
	ctx context.Context,
	request ApplyRequest,
	parsed Parsed,
	candidates []projection.Candidate,
) (ApplyResult, error) {
	paths := make([]string, len(candidates))
	kinds := make([]namespace.NodeKind, len(candidates))
	for index, candidate := range candidates {
		paths[index] = candidate.Target
		kinds[index] = namespace.ProjectionFile
	}

	var batch namespace.ProjectionBatch
	version, err := service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.batch.apply",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     paths,
			NodeKinds: kinds,
		},
	}, func(transaction *namespace.Transaction) error {
		if err := transaction.ValidatePreparedLibrary(
			ctx,
			candidates[0].Library,
		); err != nil {
			return err
		}
		for index, entry := range parsed.Document.Entries {
			if err := transaction.CheckTargetAvailable(
				ctx,
				candidates[index].Library.RootNodeID,
				entry.Target,
			); err != nil {
				return err
			}
		}

		var err error
		batch, err = transaction.CreateProjectionBatch(ctx, namespace.CreateProjectionBatch{
			LibraryID:      candidates[0].Library.ID,
			ProfileName:    candidates[0].Library.ProfileName,
			ProfileVersion: candidates[0].Library.ProfileVersion,
			Digest:         parsed.Digest,
			AppliedByUID:   request.Caller.UID,
			AppliedByGID:   request.Caller.GID,
			AppliedAt:      service.now().UTC().UnixNano(),
		})
		if err != nil {
			return err
		}
		if err := transaction.AssociateOperationWithProjectionBatch(ctx, batch.ID); err != nil {
			return err
		}

		for index, candidate := range candidates {
			parentID := candidate.Library.RootNodeID
			if len(candidate.Components) > 1 {
				parent, err := transaction.EnsureManagedDirectories(
					ctx,
					parentID,
					candidate.Components[:len(candidate.Components)-1],
					namespace.Attributes{
						Mode: candidate.Library.DirectoryMode,
						UID:  candidate.Library.UID,
						GID:  candidate.Library.GID,
					},
					request.Actor,
				)
				if err != nil {
					return err
				}
				parentID = parent.ID
			}

			node, err := transaction.CreateNode(
				ctx,
				candidate.CreateNode(parentID, request.Actor, &batch.ID),
			)
			if err != nil {
				return err
			}
			if err := transaction.CreateProjectionBatchEntry(
				ctx,
				namespace.CreateProjectionBatchEntry{
					BatchID:          batch.ID,
					Position:         int64(index),
					Source:           candidate.Source,
					Target:           parsed.Document.Entries[index].Target,
					ProjectionNodeID: node.ID,
				},
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("apply Projection Batch: %w", err)
	}
	return ApplyResult{
		Schema:           ResultSchema,
		BatchID:          formatBatchID(batch.ID),
		Digest:           parsed.Digest,
		NamespaceVersion: version,
	}, nil
}

func (service *Service) List(ctx context.Context) (ListResult, error) {
	batches, err := service.repository.ProjectionBatches(ctx)
	if err != nil {
		return ListResult{}, err
	}
	summaries := make([]BatchSummary, len(batches))
	for index, batch := range batches {
		summaries[index] = summarizeBatch(batch)
	}
	return ListResult{Schema: ListSchema, Batches: summaries}, nil
}

func (service *Service) Show(
	ctx context.Context,
	batchID string,
) (DetailResult, error) {
	id, err := parseBatchID(batchID)
	if err != nil {
		return DetailResult{}, err
	}
	batch, entries, operations, err := service.repository.ProjectionBatchDetail(ctx, id)
	if errors.Is(err, namespace.ErrNotFound) {
		return DetailResult{}, fmt.Errorf("%w: %s", ErrBatchNotFound, batchID)
	}
	if err != nil {
		return DetailResult{}, err
	}
	details := make([]DetailEntry, len(entries))
	for index, entry := range entries {
		details[index] = DetailEntry{
			Position:         entry.Position,
			Source:           sourceIdentity(entry.Source),
			Target:           batch.LibraryName + ":/" + entry.Target,
			ProjectionNodeID: entry.ProjectionNodeID,
		}
	}
	snapshots := make([]OperationSnapshot, len(operations))
	for index, operation := range operations {
		snapshots[index] = OperationSnapshot{
			Name: operation.Name,
			Actor: ActorSnapshot{
				Kind: string(operation.Actor.Kind),
				ID:   operation.Actor.ID,
			},
			NamespaceVersion: operation.NamespaceVersion,
			Details:          operation.Details,
			CreatedAtNS:      operation.CreatedAt,
		}
	}
	return DetailResult{
		Schema:     DetailSchema,
		Batch:      summarizeBatch(batch),
		Entries:    details,
		Operations: snapshots,
	}, nil
}

func (service *Service) Undo(
	ctx context.Context,
	request UndoRequest,
) (UndoResult, error) {
	id, err := parseBatchID(request.BatchID)
	if err != nil {
		return UndoResult{}, err
	}
	batch, err := service.repository.ProjectionBatch(ctx, id)
	if errors.Is(err, namespace.ErrNotFound) {
		return UndoResult{}, fmt.Errorf("%w: %s", ErrBatchNotFound, request.BatchID)
	}
	if err != nil {
		return UndoResult{}, err
	}
	if batch.Status == namespace.ProjectionBatchUndone {
		return UndoResult{}, fmt.Errorf("%w: %s", ErrAlreadyUndone, request.BatchID)
	}
	entries, err := service.repository.ProjectionBatchEntries(ctx, id)
	if err != nil {
		return UndoResult{}, err
	}
	targets := make([]string, len(entries))
	paths := make([]string, len(entries))
	kinds := make([]namespace.NodeKind, len(entries))
	for index, entry := range entries {
		targets[index] = entry.Target
		paths[index] = batch.LibraryName + ":/" + entry.Target
		kinds[index] = namespace.ProjectionFile
	}

	var retained []string
	version, err := service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.batch.undo",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     paths,
			NodeKinds: kinds,
		},
	}, func(transaction *namespace.Transaction) error {
		current, err := transaction.ProjectionBatch(ctx, id)
		if errors.Is(err, namespace.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrBatchNotFound, request.BatchID)
		}
		if err != nil {
			return err
		}
		if current.Status == namespace.ProjectionBatchUndone {
			return fmt.Errorf("%w: %s", ErrAlreadyUndone, request.BatchID)
		}
		if !current.LibraryRootAvailable {
			return fmt.Errorf(
				"%w: Library %q is not active",
				namespace.ErrLibraryNotFound,
				current.LibraryName,
			)
		}
		if err := transaction.AssociateOperationWithProjectionBatch(ctx, id); err != nil {
			return err
		}
		operations, err := transaction.ProjectionBatchOperations(ctx, id)
		if err != nil {
			return err
		}
		historicalTargets, err := projectionBatchHistoricalTargets(targets, operations)
		if err != nil {
			return err
		}
		historicalParentIDs, err := projectionTargetParentIDs(
			ctx,
			transaction,
			current.LibraryRootID,
			historicalTargets,
		)
		if err != nil {
			return err
		}

		parentIDs, currentTargets, err := transaction.RemoveProjectionBatchProjections(
			ctx,
			id,
		)
		if err != nil {
			return err
		}
		parentIDs = append(parentIDs, historicalParentIDs...)
		seenParents := make(map[int64]struct{}, len(parentIDs))
		for _, parentID := range parentIDs {
			if _, duplicate := seenParents[parentID]; duplicate {
				continue
			}
			seenParents[parentID] = struct{}{}
			if _, err := transaction.Node(ctx, parentID); errors.Is(err, namespace.ErrNotFound) {
				continue
			} else if err != nil {
				return err
			}
			if err := transaction.PruneManagedDirectories(
				ctx,
				current.LibraryRootID,
				parentID,
			); err != nil {
				return err
			}
		}
		retained, err = transaction.RetainedOverlayPaths(
			ctx,
			current.LibraryRootID,
			append(historicalTargets, currentTargets...),
		)
		if err != nil {
			return err
		}
		currentPaths := make([]string, len(currentTargets))
		currentKinds := make([]namespace.NodeKind, len(currentTargets))
		for index, target := range currentTargets {
			currentPaths[index] = current.LibraryName + ":/" + target
			currentKinds[index] = namespace.ProjectionFile
		}
		if err := transaction.SetOperationDetails(namespace.OperationDetails{
			Paths:     currentPaths,
			NodeKinds: currentKinds,
		}); err != nil {
			return err
		}
		return transaction.MarkProjectionBatchUndone(
			ctx,
			id,
			request.Caller.UID,
			request.Caller.GID,
			service.now().UTC().UnixNano(),
		)
	})
	if err != nil {
		return UndoResult{}, fmt.Errorf("undo Projection Batch: %w", err)
	}
	for index := range retained {
		retained[index] = batch.LibraryName + ":/" + retained[index]
	}
	return UndoResult{
		Schema:               UndoSchema,
		BatchID:              request.BatchID,
		Status:               string(namespace.ProjectionBatchUndone),
		NamespaceVersion:     version,
		RetainedOverlayFiles: retained,
	}, nil
}

func projectionBatchHistoricalTargets(
	entryTargets []string,
	operations []namespace.ProjectionBatchOperation,
) ([]string, error) {
	targets := append([]string(nil), entryTargets...)
	for _, operation := range operations {
		if operation.Name != "projection.move" {
			continue
		}
		if len(operation.Details.Paths) != 2 {
			return nil, fmt.Errorf(
				"%w: Projection move operation %d has %d paths",
				namespace.ErrInvalidOperation,
				operation.ID,
				len(operation.Details.Paths),
			)
		}
		for _, path := range operation.Details.Paths {
			_, relativePath, found := strings.Cut(path, ":/")
			if !found || relativePath == "" {
				return nil, fmt.Errorf(
					"%w: Projection move operation %d has invalid path %q",
					namespace.ErrInvalidOperation,
					operation.ID,
					path,
				)
			}
			components, err := namespace.ValidateRelativePath(relativePath)
			if err != nil {
				return nil, fmt.Errorf(
					"%w: Projection move operation %d path %q: %v",
					namespace.ErrInvalidOperation,
					operation.ID,
					path,
					err,
				)
			}
			targets = append(targets, strings.Join(components, "/"))
		}
	}
	return targets, nil
}

func projectionTargetParentIDs(
	ctx context.Context,
	transaction *namespace.Transaction,
	libraryRootID int64,
	targets []string,
) ([]int64, error) {
	parentIDs := make([]int64, 0, len(targets))
	for _, target := range targets {
		components, err := namespace.ValidateRelativePath(target)
		if err != nil {
			return nil, err
		}
		if len(components) == 1 {
			continue
		}
		parent, err := transaction.Lookup(
			ctx,
			libraryRootID,
			strings.Join(components[:len(components)-1], "/"),
		)
		if errors.Is(err, namespace.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		parentIDs = append(parentIDs, parent.ID)
	}
	return parentIDs, nil
}

func (service *Service) prepare(
	ctx context.Context,
	data []byte,
) (Parsed, []projection.Candidate, int64, error) {
	parsed, err := Parse(data)
	if err != nil {
		return Parsed{}, nil, 0, err
	}
	return service.prepareParsed(ctx, parsed)
}

func (service *Service) prepareParsed(
	ctx context.Context,
	parsed Parsed,
) (Parsed, []projection.Candidate, int64, error) {
	targets := make([]string, len(parsed.Document.Entries))
	for index, entry := range parsed.Document.Entries {
		targets[index] = entry.Target
	}
	library, version, err := service.repository.ValidateProjectionTargets(
		ctx,
		parsed.Document.Library,
		targets,
	)
	if err != nil {
		return Parsed{}, nil, 0, err
	}

	candidates := make([]projection.Candidate, len(parsed.Document.Entries))
	for index, entry := range parsed.Document.Entries {
		candidate, err := service.projection.PrepareForLibrary(
			ctx,
			library,
			entry.Source,
			entry.Target,
		)
		if err != nil {
			return Parsed{}, nil, 0, err
		}
		candidates[index] = candidate
	}
	return parsed, candidates, version, nil
}

func summarizeBatch(batch namespace.ProjectionBatch) BatchSummary {
	summary := BatchSummary{
		BatchID:     formatBatchID(batch.ID),
		Library:     batch.LibraryName,
		Profile:     fmt.Sprintf("%s@%d", batch.ProfileName, batch.ProfileVersion),
		Digest:      batch.Digest,
		Status:      string(batch.Status),
		AppliedBy:   Caller{UID: batch.AppliedByUID, GID: batch.AppliedByGID},
		AppliedAtNS: batch.AppliedAt,
		UndoneAtNS:  batch.UndoneAt,
	}
	if batch.UndoneByUID != nil && batch.UndoneByGID != nil {
		summary.UndoneBy = &Caller{
			UID: *batch.UndoneByUID,
			GID: *batch.UndoneByGID,
		}
	}
	return summary
}

func sourceIdentity(source namespace.ProjectionSource) SourceIdentity {
	return SourceIdentity{
		Path:          source.Path,
		DeviceID:      source.DeviceID,
		Inode:         source.Inode,
		Size:          source.Size,
		AtimeNS:       source.AtimeNS,
		MtimeNS:       source.MtimeNS,
		SourceCtimeNS: source.SourceCtimeNS,
		VerifiedAtNS:  source.VerifiedAtNS,
	}
}

func formatBatchID(id int64) string {
	return "batch-" + strconv.FormatInt(id, 10)
}

func parseBatchID(value string) (int64, error) {
	text, found := strings.CutPrefix(value, "batch-")
	if !found || text == "" {
		return 0, fmt.Errorf("%w: invalid batch ID %q", ErrBatchNotFound, value)
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != text {
		return 0, fmt.Errorf("%w: invalid batch ID %q", ErrBatchNotFound, value)
	}
	return id, nil
}
