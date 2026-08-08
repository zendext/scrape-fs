package projection

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/sourcefile"
)

type IdentityMigrationRequest struct {
	LibraryName        string
	ConfirmationDigest string
	Actor              namespace.Actor
}

type IdentityMigrationResult struct {
	Library            string
	ConfirmationDigest string
	Candidates         []IdentityMigrationCandidate
	Rejections         []IdentityMigrationRejection
	Applied            bool
}

type IdentityMigrationCandidate struct {
	Projection namespace.Projection
	Current    ObservedSource
}

type IdentityMigrationRejection struct {
	Projection namespace.Projection
	Current    *ObservedSource
	Code       string
	Message    string
}

type ObservedSource struct {
	Path          string
	Filesystem    namespace.FilesystemIdentity
	DeviceID      int64
	Inode         int64
	Size          int64
	MtimeNS       int64
	SourceCtimeNS int64
}

func (service *Service) MigrateIdentity(
	ctx context.Context,
	request IdentityMigrationRequest,
) (IdentityMigrationResult, error) {
	library, projections, err := service.repository.LegacyProjections(ctx, request.LibraryName)
	if err != nil {
		return IdentityMigrationResult{}, err
	}
	result, err := scanIdentityMigration(projections)
	if err != nil {
		return IdentityMigrationResult{}, err
	}
	result.Library = library.Name
	result.ConfirmationDigest, err = identityMigrationDigest(library, projections, result)
	if err != nil {
		return IdentityMigrationResult{}, err
	}
	if request.ConfirmationDigest == "" {
		return result, nil
	}
	if request.ConfirmationDigest != result.ConfirmationDigest {
		return IdentityMigrationResult{}, ErrConfirmationMismatch
	}
	if len(result.Candidates) == 0 {
		result.Applied = true
		return result, nil
	}

	paths := make([]string, len(result.Candidates))
	kinds := make([]namespace.NodeKind, len(result.Candidates))
	migrations := make([]namespace.ProjectionIdentityMigration, len(result.Candidates))
	for index, candidate := range result.Candidates {
		paths[index] = candidate.Projection.LibraryName + ":/" + candidate.Projection.Target
		kinds[index] = namespace.ProjectionFile
		migrations[index] = namespace.ProjectionIdentityMigration{
			ProjectionID: candidate.Projection.Node.ID,
			Filesystem:   candidate.Current.Filesystem,
		}
	}
	_, err = service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.identity.migrate",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     paths,
			NodeKinds: kinds,
		},
	}, func(transaction *namespace.Transaction) error {
		_, err := transaction.MigrateProjectionIdentities(
			ctx,
			library,
			projections,
			migrations,
		)
		return err
	})
	if errors.Is(err, namespace.ErrConflict) || errors.Is(err, namespace.ErrNotFound) {
		return IdentityMigrationResult{}, ErrConfirmationMismatch
	}
	if err != nil {
		return IdentityMigrationResult{}, fmt.Errorf(
			"migrate Projection identities for Library %q: %w",
			request.LibraryName,
			err,
		)
	}
	result.Applied = true
	return result, nil
}

func scanIdentityMigration(
	projections []namespace.Projection,
) (IdentityMigrationResult, error) {
	result := IdentityMigrationResult{
		Candidates: []IdentityMigrationCandidate{},
		Rejections: []IdentityMigrationRejection{},
	}
	for _, projection := range projections {
		if projection.Node.Source == nil || projection.Node.Source.HasFilesystem {
			return IdentityMigrationResult{}, fmt.Errorf(
				"%w: Projection %d is not legacy",
				namespace.ErrInvalidNode,
				projection.Node.ID,
			)
		}
		cached := *projection.Node.Source
		opened, err := sourcefile.Open(cached.Path)
		if err != nil {
			classified := classifySourceError(err)
			code, message := identityMigrationOpenRejection(classified)
			result.Rejections = append(result.Rejections, IdentityMigrationRejection{
				Projection: projection,
				Code:       code,
				Message:    message,
			})
			continue
		}
		current := ObservedSource{
			Path: opened.Path,
			Filesystem: namespace.FilesystemIdentity{
				Type: opened.Filesystem.Type,
				ID:   opened.Filesystem.ID,
			},
			DeviceID:      opened.DeviceID,
			Inode:         opened.Inode,
			Size:          opened.Size,
			MtimeNS:       opened.MtimeNS,
			SourceCtimeNS: opened.CtimeNS,
		}
		if err := opened.Close(); err != nil {
			result.Rejections = append(result.Rejections, IdentityMigrationRejection{
				Projection: projection,
				Current:    &current,
				Code:       "SourceUnavailable",
				Message:    "close the securely opened source failed",
			})
			continue
		}
		code, message := identityMigrationMismatch(cached, current)
		if code != "" {
			result.Rejections = append(result.Rejections, IdentityMigrationRejection{
				Projection: projection,
				Current:    &current,
				Code:       code,
				Message:    message,
			})
			continue
		}
		result.Candidates = append(result.Candidates, IdentityMigrationCandidate{
			Projection: projection,
			Current:    current,
		})
	}
	return result, nil
}

func identityMigrationMismatch(
	cached namespace.ProjectionSource,
	current ObservedSource,
) (string, string) {
	switch {
	case cached.Path != current.Path:
		return "PathMismatch", "source path no longer matches the cached path"
	case cached.Inode != current.Inode:
		return "InodeMismatch", "source inode no longer matches the cached inode"
	case cached.Size != current.Size:
		return "SizeMismatch", "source size no longer matches the cached size"
	case cached.MtimeNS != current.MtimeNS:
		return "MtimeMismatch", "source mtime no longer matches the cached mtime"
	case cached.SourceCtimeNS != current.SourceCtimeNS:
		return "CtimeMismatch", "source ctime no longer matches the cached ctime"
	default:
		return "", ""
	}
}

func identityMigrationOpenRejection(err error) (string, string) {
	switch {
	case errors.Is(err, ErrUnsafeSource):
		return "UnsafeSource", "source path cannot be opened safely"
	case errors.Is(err, ErrSourceNotFound):
		return "SourceNotFound", "source path does not exist"
	case errors.Is(err, ErrSourcePermissionDenied):
		return "SourcePermissionDenied", "source path cannot be read"
	case errors.Is(err, ErrSourceNotRegular):
		return "SourceNotRegular", "source path is not a regular file"
	case errors.Is(err, fs.ErrNotExist):
		return "SourceNotFound", "source path does not exist"
	default:
		return "SourceUnavailable", "source path cannot be inspected"
	}
}

func identityMigrationDigest(
	library namespace.Library,
	projections []namespace.Projection,
	result IdentityMigrationResult,
) (string, error) {
	type digestSource struct {
		Path          string `json:"path"`
		DeviceID      int64  `json:"device_id"`
		Inode         int64  `json:"inode"`
		Size          int64  `json:"size"`
		AtimeNS       int64  `json:"atime_ns"`
		MtimeNS       int64  `json:"mtime_ns"`
		SourceCtimeNS int64  `json:"source_ctime_ns"`
		VerifiedAtNS  int64  `json:"verified_at_ns"`
	}
	type digestObserved struct {
		Path          string                       `json:"path"`
		Filesystem    namespace.FilesystemIdentity `json:"filesystem"`
		DeviceID      int64                        `json:"device_id"`
		Inode         int64                        `json:"inode"`
		Size          int64                        `json:"size"`
		MtimeNS       int64                        `json:"mtime_ns"`
		SourceCtimeNS int64                        `json:"source_ctime_ns"`
	}
	type digestRecord struct {
		ProjectionID  int64           `json:"projection_id"`
		ParentID      int64           `json:"parent_id"`
		Target        string          `json:"target"`
		OriginBatchID *int64          `json:"origin_batch_id"`
		Mode          uint32          `json:"mode"`
		UID           uint32          `json:"uid"`
		GID           uint32          `json:"gid"`
		VirtualCtime  int64           `json:"virtual_ctime_ns"`
		UpdatedAt     int64           `json:"updated_at_ns"`
		Cached        digestSource    `json:"cached_source"`
		Current       *digestObserved `json:"current_source,omitempty"`
		Outcome       string          `json:"outcome"`
		RejectionCode string          `json:"rejection_code,omitempty"`
	}
	recordByID := make(map[int64]digestRecord, len(projections))
	for _, projection := range projections {
		source := *projection.Node.Source
		parentID := int64(0)
		if projection.Node.ParentID != nil {
			parentID = *projection.Node.ParentID
		}
		recordByID[projection.Node.ID] = digestRecord{
			ProjectionID:  projection.Node.ID,
			ParentID:      parentID,
			Target:        projection.Target,
			OriginBatchID: projection.Node.OriginScrapeID,
			Mode:          uint32(projection.Node.Attributes.Mode.Perm()),
			UID:           projection.Node.Attributes.UID,
			GID:           projection.Node.Attributes.GID,
			VirtualCtime:  projection.Node.VirtualCtimeNS,
			UpdatedAt:     projection.Node.UpdatedAt,
			Cached: digestSource{
				Path:          source.Path,
				DeviceID:      source.DeviceID,
				Inode:         source.Inode,
				Size:          source.Size,
				AtimeNS:       source.AtimeNS,
				MtimeNS:       source.MtimeNS,
				SourceCtimeNS: source.SourceCtimeNS,
				VerifiedAtNS:  source.VerifiedAtNS,
			},
		}
	}
	observed := func(source ObservedSource) *digestObserved {
		return &digestObserved{
			Path:          source.Path,
			Filesystem:    source.Filesystem,
			DeviceID:      source.DeviceID,
			Inode:         source.Inode,
			Size:          source.Size,
			MtimeNS:       source.MtimeNS,
			SourceCtimeNS: source.SourceCtimeNS,
		}
	}
	for _, candidate := range result.Candidates {
		record := recordByID[candidate.Projection.Node.ID]
		record.Current = observed(candidate.Current)
		record.Outcome = "candidate"
		recordByID[candidate.Projection.Node.ID] = record
	}
	for _, rejection := range result.Rejections {
		record := recordByID[rejection.Projection.Node.ID]
		if rejection.Current != nil {
			record.Current = observed(*rejection.Current)
		}
		record.Outcome = "rejected"
		record.RejectionCode = rejection.Code
		recordByID[rejection.Projection.Node.ID] = record
	}
	records := make([]digestRecord, len(projections))
	for index, projection := range projections {
		records[index] = recordByID[projection.Node.ID]
	}
	encoded, err := json.Marshal(struct {
		Schema        string         `json:"schema"`
		LibraryID     int64          `json:"library_id"`
		LibraryName   string         `json:"library_name"`
		LibraryRootID int64          `json:"library_root_id"`
		Records       []digestRecord `json:"records"`
	}{
		Schema:        "scrapefs.projection-identity-migration-confirmation/v1",
		LibraryID:     library.ID,
		LibraryName:   library.Name,
		LibraryRootID: library.RootNodeID,
		Records:       records,
	})
	if err != nil {
		return "", fmt.Errorf("encode Projection identity migration confirmation: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum), nil
}
