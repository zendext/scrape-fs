package projection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/sourcefile"
)

var (
	ErrLibraryNotFound  = namespace.ErrLibraryNotFound
	ErrInvalidPath      = namespace.ErrInvalidPath
	ErrTargetConflict   = namespace.ErrConflict
	ErrUnsafeSource     = errors.New("unsafe projection source")
	ErrSourceNotRegular = errors.New("projection source is not a regular file")
)

type CreateRequest struct {
	SourcePath string
	Target     string
	Actor      namespace.Actor
}

type Service struct {
	repository *namespace.Repository
	now        func() time.Time
}

func NewService(repository *namespace.Repository) *Service {
	return &Service{
		repository: repository,
		now:        time.Now,
	}
}

func (service *Service) Create(
	ctx context.Context,
	request CreateRequest,
) (namespace.Node, error) {
	candidate, err := service.Prepare(ctx, request)
	if err != nil {
		return namespace.Node{}, err
	}

	var created namespace.Node
	_, err = service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.create",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     []string{candidate.Target},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
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

		var err error
		created, err = transaction.CreateNode(
			ctx,
			candidate.CreateNode(parentID, request.Actor, nil),
		)
		return err
	})
	if err != nil {
		return namespace.Node{}, fmt.Errorf(
			"create Projection %q: %w",
			candidate.Target,
			err,
		)
	}
	return created, nil
}

type Candidate struct {
	Library    namespace.Library
	Components []string
	Source     namespace.ProjectionSource
	Target     string
}

func (service *Service) Prepare(
	ctx context.Context,
	request CreateRequest,
) (Candidate, error) {
	libraryName, relativePath, err := parseTarget(request.Target)
	if err != nil {
		return Candidate{}, err
	}

	library, err := service.repository.Library(ctx, libraryName)
	if err != nil {
		return Candidate{}, err
	}
	return service.PrepareForLibrary(ctx, library, request.SourcePath, relativePath)
}

func (service *Service) PrepareForLibrary(
	ctx context.Context,
	library namespace.Library,
	sourcePath string,
	relativePath string,
) (Candidate, error) {
	components, err := namespace.ValidateRelativePath(relativePath)
	if err != nil {
		return Candidate{}, err
	}
	if err := validateProfilePath(
		library.ProfileName,
		library.ProfileVersion,
		components,
	); err != nil {
		return Candidate{}, err
	}

	opened, err := sourcefile.Open(sourcePath)
	if err != nil {
		return Candidate{}, classifySourceError(err)
	}
	metadata := opened.Metadata
	if err := opened.Close(); err != nil {
		return Candidate{}, fmt.Errorf(
			"close Projection source %q: %w",
			metadata.Path,
			err,
		)
	}

	return Candidate{
		Library:    library,
		Components: components,
		Target:     library.Name + ":/" + relativePath,
		Source: namespace.ProjectionSource{
			Path:          metadata.Path,
			DeviceID:      metadata.DeviceID,
			Inode:         metadata.Inode,
			Size:          metadata.Size,
			AtimeNS:       metadata.AtimeNS,
			MtimeNS:       metadata.MtimeNS,
			SourceCtimeNS: metadata.CtimeNS,
			VerifiedAtNS:  service.now().UTC().UnixNano(),
		},
	}, nil
}

func (candidate Candidate) CreateNode(
	parentID int64,
	creator namespace.Actor,
	originScrapeID *int64,
) namespace.CreateNode {
	return namespace.CreateNode{
		ParentID:       parentID,
		Name:           candidate.Components[len(candidate.Components)-1],
		Kind:           namespace.ProjectionFile,
		Source:         &candidate.Source,
		OriginScrapeID: originScrapeID,
		Creator:        creator,
		Attributes: namespace.Attributes{
			Mode: candidate.Library.ProjectionMode,
			UID:  candidate.Library.UID,
			GID:  candidate.Library.GID,
		},
	}
}

func parseTarget(value string) (string, string, error) {
	libraryName, relativePath, found := strings.Cut(value, ":/")
	if !found || libraryName == "" || relativePath == "" {
		return "", "", fmt.Errorf(
			"%w: target must use <library-name>:/<relative-path>",
			ErrInvalidPath,
		)
	}

	libraryComponents, err := namespace.ValidateRelativePath(libraryName)
	if err != nil || len(libraryComponents) != 1 {
		return "", "", fmt.Errorf(
			"%w: invalid target Library name %q",
			ErrInvalidPath,
			libraryName,
		)
	}
	if _, err := namespace.ValidateRelativePath(relativePath); err != nil {
		return "", "", err
	}
	return libraryName, relativePath, nil
}

func validateProfilePath(
	profileName string,
	profileVersion int64,
	components []string,
) error {
	switch {
	case profileName == "jellyfin" && profileVersion == 1,
		profileName == "plex" && profileVersion == 1:
	default:
		return fmt.Errorf(
			"%w: %s@%d",
			namespace.ErrProfileNotFound,
			profileName,
			profileVersion,
		)
	}

	for _, component := range components {
		if strings.IndexFunc(component, unicode.IsControl) >= 0 {
			return fmt.Errorf(
				"%w: %s@%d path component %q contains a control character",
				ErrInvalidPath,
				profileName,
				profileVersion,
				component,
			)
		}
		if profileName == "jellyfin" {
			if index := strings.IndexAny(component, `<>:"\|?*`); index >= 0 {
				return fmt.Errorf(
					"%w: %s@%d path component %q contains reserved character %q",
					ErrInvalidPath,
					profileName,
					profileVersion,
					component,
					component[index],
				)
			}
		}
	}
	return nil
}

func classifySourceError(err error) error {
	switch {
	case errors.Is(err, sourcefile.ErrUnsafePath):
		return fmt.Errorf("%w: %w", ErrUnsafeSource, err)
	case errors.Is(err, sourcefile.ErrNotRegular):
		return fmt.Errorf("%w: %w", ErrSourceNotRegular, err)
	default:
		return err
	}
}
