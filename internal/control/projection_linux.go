package control

import (
	"context"
	"strings"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
)

const (
	ProjectionResultSchema = "scrapefs.projection-result/v1"
	ProjectionListSchema   = "scrapefs.projection-list/v1"
)

type ProjectionService interface {
	Create(context.Context, projection.CreateRequest) (namespace.Node, error)
	List(context.Context, projection.ListRequest) ([]namespace.Projection, error)
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
	Path          string `json:"path"`
	DeviceID      int64  `json:"device_id"`
	Inode         int64  `json:"inode"`
	Size          int64  `json:"size"`
	AtimeNS       int64  `json:"atime_ns"`
	MtimeNS       int64  `json:"mtime_ns"`
	SourceCtimeNS int64  `json:"source_ctime_ns"`
	VerifiedAtNS  int64  `json:"verified_at_ns"`
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
		source = ProjectionSourceSnapshot{
			Path:          record.Node.Source.Path,
			DeviceID:      record.Node.Source.DeviceID,
			Inode:         record.Node.Source.Inode,
			Size:          record.Node.Source.Size,
			AtimeNS:       record.Node.Source.AtimeNS,
			MtimeNS:       record.Node.Source.MtimeNS,
			SourceCtimeNS: record.Node.Source.SourceCtimeNS,
			VerifiedAtNS:  record.Node.Source.VerifiedAtNS,
		}
	}
	return ProjectionSnapshot{
		ID:      record.Node.ID,
		Library: record.LibraryName,
		Target:  record.LibraryName + ":/" + record.Target,
		Source:  source,
	}
}
