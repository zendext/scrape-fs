package control

import (
	"context"
	"strings"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
)

const (
	ProjectionResultSchema                  = "scrapefs.projection-result/v1"
	ProjectionListSchema                    = "scrapefs.projection-list/v1"
	ProjectionRebindResultSchema            = "scrapefs.projection-rebind-result/v1"
	ProjectionIdentityMigrationResultSchema = "scrapefs.projection-identity-migration-result/v1"
)

type ProjectionService interface {
	Create(context.Context, projection.CreateRequest) (namespace.Node, error)
	List(context.Context, projection.ListRequest) ([]namespace.Projection, error)
	Move(context.Context, projection.MoveRequest) (namespace.Projection, error)
	Remove(context.Context, projection.RemoveRequest) (namespace.Projection, error)
	Rebind(context.Context, projection.RebindRequest) (projection.RebindResult, error)
	MigrateIdentity(context.Context, projection.IdentityMigrationRequest) (projection.IdentityMigrationResult, error)
}

type ProjectionResult struct {
	Schema     string             `json:"schema"`
	Projection ProjectionSnapshot `json:"projection"`
}

type ProjectionListResult struct {
	Schema      string               `json:"schema"`
	Projections []ProjectionSnapshot `json:"projections"`
}

type ProjectionSnapshot struct {
	ID      int64                    `json:"id"`
	Library string                   `json:"library"`
	Target  string                   `json:"target"`
	Source  ProjectionSourceSnapshot `json:"source"`
}

type ProjectionSourceSnapshot struct {
	Path          string                        `json:"path"`
	Filesystem    *namespace.FilesystemIdentity `json:"filesystem,omitempty"`
	DeviceID      int64                         `json:"device_id"`
	Inode         int64                         `json:"inode"`
	Size          int64                         `json:"size"`
	AtimeNS       int64                         `json:"atime_ns"`
	MtimeNS       int64                         `json:"mtime_ns"`
	SourceCtimeNS int64                         `json:"source_ctime_ns"`
	VerifiedAtNS  int64                         `json:"verified_at_ns"`
}

type ProjectionRebindStatus string

const (
	ProjectionRebindConfirmationRequired ProjectionRebindStatus = "confirmation_required"
	ProjectionRebindApplied              ProjectionRebindStatus = "applied"
)

type ProjectionRebindResult struct {
	Schema             string                   `json:"schema"`
	Status             ProjectionRebindStatus   `json:"status"`
	ConfirmationDigest string                   `json:"confirmation_digest"`
	Projection         ProjectionSnapshot       `json:"projection"`
	PreviousSource     ProjectionSourceSnapshot `json:"previous_source"`
	ProposedSource     ProjectionSourceSnapshot `json:"proposed_source"`
}

type ProjectionIdentityMigrationStatus string

const (
	ProjectionIdentityMigrationConfirmationRequired ProjectionIdentityMigrationStatus = "confirmation_required"
	ProjectionIdentityMigrationApplied              ProjectionIdentityMigrationStatus = "applied"
)

type ProjectionIdentityMigrationResult struct {
	Schema             string                                 `json:"schema"`
	Status             ProjectionIdentityMigrationStatus      `json:"status"`
	Library            string                                 `json:"library"`
	ConfirmationDigest string                                 `json:"confirmation_digest"`
	Candidates         []ProjectionIdentityMigrationCandidate `json:"candidates"`
	Rejections         []ProjectionIdentityMigrationRejection `json:"rejections"`
}

type ProjectionIdentityMigrationCandidate struct {
	Projection    ProjectionSnapshot               `json:"projection"`
	CurrentSource ProjectionIdentityObservedSource `json:"current_source"`
}

type ProjectionIdentityMigrationRejection struct {
	Projection    ProjectionSnapshot                `json:"projection"`
	CurrentSource *ProjectionIdentityObservedSource `json:"current_source,omitempty"`
	Code          string                            `json:"code"`
	Message       string                            `json:"message"`
}

type ProjectionIdentityObservedSource struct {
	Path          string                       `json:"path"`
	Filesystem    namespace.FilesystemIdentity `json:"filesystem"`
	DeviceID      int64                        `json:"device_id"`
	Inode         int64                        `json:"inode"`
	Size          int64                        `json:"size"`
	MtimeNS       int64                        `json:"mtime_ns"`
	SourceCtimeNS int64                        `json:"source_ctime_ns"`
}

func (client *Client) CreateProjection(
	ctx context.Context,
	create projection.CreateRequest,
) (ProjectionResult, error) {
	var result ProjectionResult
	err := client.call(ctx, request{
		Schema:     requestSchema,
		Command:    "projection.create",
		SourcePath: create.SourcePath,
		Target:     create.Target,
	}, &result)
	return result, err
}

func (client *Client) ListProjections(
	ctx context.Context,
	libraryName string,
) (ProjectionListResult, error) {
	var result ProjectionListResult
	err := client.call(ctx, request{
		Schema:      requestSchema,
		Command:     "projection.list",
		LibraryName: libraryName,
	}, &result)
	return result, err
}

func (client *Client) MoveProjection(
	ctx context.Context,
	move projection.MoveRequest,
) (ProjectionResult, error) {
	var result ProjectionResult
	err := client.call(ctx, request{
		Schema:    requestSchema,
		Command:   "projection.move",
		Target:    move.Target,
		NewTarget: move.NewTarget,
	}, &result)
	return result, err
}

func (client *Client) RemoveProjection(
	ctx context.Context,
	remove projection.RemoveRequest,
) (ProjectionResult, error) {
	var result ProjectionResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "projection.remove",
		Target:  remove.Target,
	}, &result)
	return result, err
}

func (client *Client) RebindProjection(
	ctx context.Context,
	rebind projection.RebindRequest,
) (ProjectionRebindResult, error) {
	var result ProjectionRebindResult
	err := client.call(ctx, request{
		Schema:             requestSchema,
		Command:            "projection.rebind",
		Target:             rebind.Target,
		SourcePath:         rebind.SourcePath,
		ConfirmationDigest: rebind.ConfirmationDigest,
	}, &result)
	return result, err
}

func (client *Client) MigrateProjectionIdentity(
	ctx context.Context,
	migrate projection.IdentityMigrationRequest,
) (ProjectionIdentityMigrationResult, error) {
	var result ProjectionIdentityMigrationResult
	err := client.call(ctx, request{
		Schema:             requestSchema,
		Command:            "projection.identity.migrate",
		LibraryName:        migrate.LibraryName,
		ConfirmationDigest: migrate.ConfirmationDigest,
	}, &result)
	return result, err
}

func projectionResult(node namespace.Node, target string) ProjectionResult {
	libraryName, _, _ := strings.Cut(target, ":/")
	return ProjectionResult{
		Schema: ProjectionResultSchema,
		Projection: snapshotProjection(namespace.Projection{
			LibraryName: libraryName,
			Target:      strings.TrimPrefix(target, libraryName+":/"),
			Node:        node,
		}),
	}
}

func projectionRecordResult(record namespace.Projection) ProjectionResult {
	return ProjectionResult{
		Schema:     ProjectionResultSchema,
		Projection: snapshotProjection(record),
	}
}

func projectionRebindResult(result projection.RebindResult) ProjectionRebindResult {
	status := ProjectionRebindConfirmationRequired
	if result.Applied {
		status = ProjectionRebindApplied
	}
	return ProjectionRebindResult{
		Schema:             ProjectionRebindResultSchema,
		Status:             status,
		ConfirmationDigest: result.ConfirmationDigest,
		Projection:         snapshotProjection(result.Projection),
		PreviousSource:     snapshotProjectionSource(result.PreviousSource),
		ProposedSource:     snapshotProjectionSource(result.ProposedSource),
	}
}

func projectionIdentityMigrationResult(
	result projection.IdentityMigrationResult,
) ProjectionIdentityMigrationResult {
	status := ProjectionIdentityMigrationConfirmationRequired
	if result.Applied {
		status = ProjectionIdentityMigrationApplied
	}
	candidates := make([]ProjectionIdentityMigrationCandidate, len(result.Candidates))
	for index, candidate := range result.Candidates {
		candidates[index] = ProjectionIdentityMigrationCandidate{
			Projection:    snapshotProjection(candidate.Projection),
			CurrentSource: snapshotObservedSource(candidate.Current),
		}
	}
	rejections := make([]ProjectionIdentityMigrationRejection, len(result.Rejections))
	for index, rejection := range result.Rejections {
		rejections[index] = ProjectionIdentityMigrationRejection{
			Projection: snapshotProjection(rejection.Projection),
			Code:       rejection.Code,
			Message:    rejection.Message,
		}
		if rejection.Current != nil {
			current := snapshotObservedSource(*rejection.Current)
			rejections[index].CurrentSource = &current
		}
	}
	return ProjectionIdentityMigrationResult{
		Schema:             ProjectionIdentityMigrationResultSchema,
		Status:             status,
		Library:            result.Library,
		ConfirmationDigest: result.ConfirmationDigest,
		Candidates:         candidates,
		Rejections:         rejections,
	}
}

func snapshotObservedSource(source projection.ObservedSource) ProjectionIdentityObservedSource {
	return ProjectionIdentityObservedSource{
		Path:          source.Path,
		Filesystem:    source.Filesystem,
		DeviceID:      source.DeviceID,
		Inode:         source.Inode,
		Size:          source.Size,
		MtimeNS:       source.MtimeNS,
		SourceCtimeNS: source.SourceCtimeNS,
	}
}

func projectionListResult(records []namespace.Projection) ProjectionListResult {
	projections := make([]ProjectionSnapshot, len(records))
	for index, record := range records {
		projections[index] = snapshotProjection(record)
	}
	return ProjectionListResult{
		Schema:      ProjectionListSchema,
		Projections: projections,
	}
}

func snapshotProjection(record namespace.Projection) ProjectionSnapshot {
	source := ProjectionSourceSnapshot{}
	if record.Node.Source != nil {
		source = snapshotProjectionSource(*record.Node.Source)
	}
	return ProjectionSnapshot{
		ID:      record.Node.ID,
		Library: record.LibraryName,
		Target:  record.LibraryName + ":/" + record.Target,
		Source:  source,
	}
}

func snapshotProjectionSource(source namespace.ProjectionSource) ProjectionSourceSnapshot {
	snapshot := ProjectionSourceSnapshot{
		Path:          source.Path,
		DeviceID:      source.DeviceID,
		Inode:         source.Inode,
		Size:          source.Size,
		AtimeNS:       source.AtimeNS,
		MtimeNS:       source.MtimeNS,
		SourceCtimeNS: source.SourceCtimeNS,
		VerifiedAtNS:  source.VerifiedAtNS,
	}
	if source.HasFilesystem {
		filesystem := source.Filesystem
		snapshot.Filesystem = &filesystem
	}
	return snapshot
}
