package control

import (
	"context"
	"io/fs"

	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
)

const (
	LibraryResultSchema = "scrapefs.library-result/v1"
	LibraryListSchema   = "scrapefs.library-list/v1"
)

type LibraryService interface {
	Create(context.Context, library.CreateRequest) (namespace.Library, error)
	List(context.Context) ([]namespace.Library, error)
}

type LibraryResult struct {
	Schema  string          `json:"schema"`
	Library LibrarySnapshot `json:"library"`
}

type LibraryListResult struct {
	Schema    string            `json:"schema"`
	Libraries []LibrarySnapshot `json:"libraries"`
}

type LibrarySnapshot struct {
	ID             int64                   `json:"id"`
	Name           string                  `json:"name"`
	Status         namespace.LibraryStatus `json:"status"`
	RootNodeID     int64                   `json:"root_node_id"`
	ProfileName    string                  `json:"profile_name"`
	ProfileVersion int64                   `json:"profile_version"`
	UID            uint32                  `json:"uid"`
	GID            uint32                  `json:"gid"`
	RootMode       fs.FileMode             `json:"root_mode"`
	DirectoryMode  fs.FileMode             `json:"directory_mode"`
	ProjectionMode fs.FileMode             `json:"projection_mode"`
	OverlayMode    fs.FileMode             `json:"overlay_mode"`
	CreatedAtNS    int64                   `json:"created_at_ns"`
	UpdatedAtNS    int64                   `json:"updated_at_ns"`
}

func (client *Client) CreateLibrary(
	ctx context.Context,
	name string,
	profile library.ProfileIdentifier,
) (LibraryResult, error) {
	var result LibraryResult
	err := client.call(ctx, request{
		Schema:         requestSchema,
		Command:        "library.create",
		LibraryName:    name,
		ProfileName:    profile.Name,
		ProfileVersion: profile.Version,
	}, &result)
	return result, err
}

func (client *Client) ListLibraries(
	ctx context.Context,
) (LibraryListResult, error) {
	var result LibraryListResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "library.list",
	}, &result)
	return result, err
}

func libraryResult(record namespace.Library) LibraryResult {
	return LibraryResult{
		Schema:  LibraryResultSchema,
		Library: snapshotLibrary(record),
	}
}

func libraryListResult(records []namespace.Library) LibraryListResult {
	libraries := make([]LibrarySnapshot, len(records))
	for index, record := range records {
		libraries[index] = snapshotLibrary(record)
	}
	return LibraryListResult{
		Schema:    LibraryListSchema,
		Libraries: libraries,
	}
}

func snapshotLibrary(record namespace.Library) LibrarySnapshot {
	return LibrarySnapshot{
		ID:             record.ID,
		Name:           record.Name,
		Status:         record.Status,
		RootNodeID:     record.RootNodeID,
		ProfileName:    record.ProfileName,
		ProfileVersion: record.ProfileVersion,
		UID:            record.UID,
		GID:            record.GID,
		RootMode:       record.RootMode,
		DirectoryMode:  record.DirectoryMode,
		ProjectionMode: record.ProjectionMode,
		OverlayMode:    record.OverlayMode,
		CreatedAtNS:    record.CreatedAt,
		UpdatedAtNS:    record.UpdatedAt,
	}
}
