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
	candidate, err := service.prepare(ctx, request)
	if err != nil {
		return namespace.Node{}, err
	}

	var created namespace.Node
	_, err = service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.create",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     []string{candidate.target},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
		parentID := candidate.library.RootNodeID
		if len(candidate.components) > 1 {
			parent, err := transaction.EnsureManagedDirectories(
				ctx,
				parentID,
				candidate.components[:len(candidate.components)-1],
				namespace.Attributes{
					Mode: candidate.library.DirectoryMode,
					UID:  candidate.library.UID,
					GID:  candidate.library.GID,
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
			candidate.createNode(parentID, request.Actor),
		)
		return err
	})
	if err != nil {
		return namespace.Node{}, fmt.Errorf(
			"create Projection %q: %w",
			candidate.target,
			err,
		)
	}
	return created, nil
}

type projectionCandidate struct {
	library    namespace.Library
	components []string
	source     namespace.ProjectionSource
	target     string
}

func (service *Service) prepare(
	ctx context.Context,
	request CreateRequest,
) (projectionCandidate, error) {
	libraryName, relativePath, components, err := parseTarget(request.Target)
	if err != nil {
		return projectionCandidate{}, err
	}

	library, err := service.repository.Library(ctx, libraryName)
	if err != nil {
		return projectionCandidate{}, err
	}
	if err := validateProfilePath(
		library.ProfileName,
		library.ProfileVersion,
		components,
	); err != nil {
		return projectionCandidate{}, err
	}

	opened, err := sourcefile.Open(request.SourcePath)
	if err != nil {
		return projectionCandidate{}, classifySourceError(err)
	}
	metadata := opened.Metadata
	if err := opened.Close(); err != nil {
		return projectionCandidate{}, fmt.Errorf(
			"close Projection source %q: %w",
			metadata.Path,
			err,
		)
	}

	return projectionCandidate{
		library:    library,
		components: components,
		target:     library.Name + ":/" + relativePath,
		source: namespace.ProjectionSource{
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

func (candidate projectionCandidate) createNode(
	parentID int64,
	creator namespace.Actor,
) namespace.CreateNode {
	return namespace.CreateNode{
		ParentID: parentID,
		Name:     candidate.components[len(candidate.components)-1],
		Kind:     namespace.ProjectionFile,
		Source:   &candidate.source,
		Creator:  creator,
		Attributes: namespace.Attributes{
			Mode: candidate.library.ProjectionMode,
			UID:  candidate.library.UID,
			GID:  candidate.library.GID,
		},
	}
}

func parseTarget(value string) (string, string, []string, error) {
	libraryName, relativePath, found := strings.Cut(value, ":/")
	if !found || libraryName == "" || relativePath == "" {
		return "", "", nil, fmt.Errorf(
			"%w: target must use <library-name>:/<relative-path>",
			ErrInvalidPath,
		)
	}

	libraryComponents, err := namespace.ValidateRelativePath(libraryName)
	if err != nil || len(libraryComponents) != 1 {
		return "", "", nil, fmt.Errorf(
			"%w: invalid target Library name %q",
			ErrInvalidPath,
			libraryName,
		)
	}
	components, err := namespace.ValidateRelativePath(relativePath)
	if err != nil {
		return "", "", nil, err
	}
	return libraryName, relativePath, components, nil
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
