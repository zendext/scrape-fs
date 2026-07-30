package projection

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/sourcefile"
)

var (
	ErrLibraryNotFound        = namespace.ErrLibraryNotFound
	ErrInvalidPath            = namespace.ErrInvalidPath
	ErrTargetConflict         = namespace.ErrConflict
	ErrUnsafeSource           = errors.New("unsafe projection source")
	ErrSourceNotFound         = errors.New("projection source not found")
	ErrSourcePermissionDenied = errors.New("projection source permission denied")
	ErrSourceNotRegular       = errors.New("projection source is not a regular file")
	ErrProjectionNotFound     = errors.New("projection not found")
	ErrConfirmationMismatch   = errors.New("projection rebind confirmation mismatch")
	ErrBatchOwnedProjection   = namespace.ErrBatchOwnedProjection
)

type CreateRequest struct {
	SourcePath string
	Target     string
	Actor      namespace.Actor
}

type ListRequest struct {
	LibraryName string
}

type MoveRequest struct {
	Target    string
	NewTarget string
	Actor     namespace.Actor
}

type RemoveRequest struct {
	Target string
	Actor  namespace.Actor
}

type RebindRequest struct {
	Target             string
	SourcePath         string
	ConfirmationDigest string
	Actor              namespace.Actor
}

type RebindResult struct {
	Projection         namespace.Projection
	PreviousSource     namespace.ProjectionSource
	ProposedSource     namespace.ProjectionSource
	ConfirmationDigest string
	Applied            bool
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

func (service *Service) List(
	ctx context.Context,
	request ListRequest,
) ([]namespace.Projection, error) {
	return service.repository.Projections(ctx, request.LibraryName)
}

func (service *Service) Create(
	ctx context.Context,
	request CreateRequest,
) (namespace.Node, error) {
	candidate, err := service.Prepare(ctx, request)
	if err != nil {
		return namespace.Node{}, err
	}
	return service.createPrepared(ctx, request, candidate)
}

func (service *Service) Move(
	ctx context.Context,
	request MoveRequest,
) (namespace.Projection, error) {
	sourceLibraryName, sourceRelativePath, sourceComponents, err := parseLifecycleTarget(
		request.Target,
	)
	if err != nil {
		return namespace.Projection{}, err
	}
	targetLibraryName, targetRelativePath, targetComponents, err := parseLifecycleTarget(
		request.NewTarget,
	)
	if err != nil {
		return namespace.Projection{}, err
	}
	if sourceLibraryName != targetLibraryName {
		return namespace.Projection{}, namespace.ErrCrossLibrary
	}
	library, source, err := service.projectionAt(
		ctx,
		sourceLibraryName,
		sourceRelativePath,
	)
	if err != nil {
		return namespace.Projection{}, err
	}
	if err := validateProfilePath(
		library.ProfileName,
		library.ProfileVersion,
		targetComponents,
	); err != nil {
		return namespace.Projection{}, err
	}

	var moved namespace.Node
	_, err = service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.move",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     []string{request.Target, request.NewTarget},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile, namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
		if err := transaction.ValidatePreparedLibrary(ctx, library); err != nil {
			return err
		}
		oldParent, err := projectionParent(
			ctx,
			transaction,
			library.RootNodeID,
			sourceComponents,
			namespace.Attributes{},
			request.Actor,
			false,
		)
		if err != nil {
			return err
		}
		newParent, err := projectionParent(
			ctx,
			transaction,
			library.RootNodeID,
			targetComponents,
			namespace.Attributes{
				Mode: library.DirectoryMode,
				UID:  library.UID,
				GID:  library.GID,
			},
			request.Actor,
			true,
		)
		if err != nil {
			return err
		}
		moved, err = transaction.MoveProjection(ctx, namespace.MoveProjectionNode{
			OldParentID: oldParent.ID,
			OldName:     sourceComponents[len(sourceComponents)-1],
			NewParentID: newParent.ID,
			NewName:     targetComponents[len(targetComponents)-1],
			ExpectedID:  source.ID,
		})
		return err
	})
	if errors.Is(err, namespace.ErrNotFound) || errors.Is(err, namespace.ErrReadOnly) {
		return namespace.Projection{}, fmt.Errorf(
			"%w: %s:/%s",
			ErrProjectionNotFound,
			sourceLibraryName,
			sourceRelativePath,
		)
	}
	if err != nil {
		return namespace.Projection{}, fmt.Errorf(
			"move Projection %q to %q: %w",
			request.Target,
			request.NewTarget,
			err,
		)
	}
	return namespace.Projection{
		LibraryID:   library.ID,
		LibraryName: library.Name,
		Target:      targetRelativePath,
		Node:        moved,
	}, nil
}

func (service *Service) Remove(
	ctx context.Context,
	request RemoveRequest,
) (namespace.Projection, error) {
	libraryName, relativePath, components, err := parseLifecycleTarget(request.Target)
	if err != nil {
		return namespace.Projection{}, err
	}
	library, source, err := service.projectionAt(ctx, libraryName, relativePath)
	if err != nil {
		return namespace.Projection{}, err
	}

	var removed namespace.Node
	_, err = service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.remove",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     []string{request.Target},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
		if err := transaction.ValidatePreparedLibrary(ctx, library); err != nil {
			return err
		}
		parent, err := projectionParent(
			ctx,
			transaction,
			library.RootNodeID,
			components,
			namespace.Attributes{},
			request.Actor,
			false,
		)
		if err != nil {
			return err
		}
		removed, err = transaction.RemoveProjection(ctx, namespace.RemoveProjectionNode{
			ParentID:   parent.ID,
			Name:       components[len(components)-1],
			ExpectedID: source.ID,
		})
		if err != nil {
			return err
		}
		return transaction.PruneManagedDirectories(ctx, library.RootNodeID, parent.ID)
	})
	if errors.Is(err, namespace.ErrNotFound) || errors.Is(err, namespace.ErrReadOnly) {
		return namespace.Projection{}, fmt.Errorf(
			"%w: %s:/%s",
			ErrProjectionNotFound,
			libraryName,
			relativePath,
		)
	}
	if err != nil {
		return namespace.Projection{}, fmt.Errorf("remove Projection %q: %w", request.Target, err)
	}
	return namespace.Projection{
		LibraryID:   library.ID,
		LibraryName: library.Name,
		Target:      relativePath,
		Node:        removed,
	}, nil
}

func (service *Service) Rebind(
	ctx context.Context,
	request RebindRequest,
) (RebindResult, error) {
	libraryName, relativePath, components, err := parseLifecycleTarget(request.Target)
	if err != nil {
		return RebindResult{}, err
	}
	library, current, err := service.projectionAt(ctx, libraryName, relativePath)
	if err != nil {
		return RebindResult{}, err
	}
	if current.OriginScrapeID != nil {
		return RebindResult{}, fmt.Errorf(
			"%w: %s:/%s",
			ErrBatchOwnedProjection,
			libraryName,
			relativePath,
		)
	}
	proposed, err := service.projectionSource(request.SourcePath)
	if err != nil {
		return RebindResult{}, err
	}
	digest, err := rebindConfirmationDigest(
		current.ID,
		request.Target,
		*current.Source,
		proposed,
	)
	if err != nil {
		return RebindResult{}, err
	}
	result := RebindResult{
		Projection: namespace.Projection{
			LibraryID:   library.ID,
			LibraryName: library.Name,
			Target:      relativePath,
			Node:        current,
		},
		PreviousSource:     *current.Source,
		ProposedSource:     proposed,
		ConfirmationDigest: digest,
	}
	if request.ConfirmationDigest == "" {
		return result, nil
	}
	if request.ConfirmationDigest != digest {
		return RebindResult{}, ErrConfirmationMismatch
	}

	_, err = service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.rebind",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     []string{request.Target},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
		if err := transaction.ValidatePreparedLibrary(ctx, library); err != nil {
			return err
		}
		parent, err := projectionParent(
			ctx,
			transaction,
			library.RootNodeID,
			components,
			namespace.Attributes{},
			request.Actor,
			false,
		)
		if err != nil {
			return err
		}
		result.Projection.Node, err = transaction.RebindProjection(
			ctx,
			namespace.RebindProjectionNode{
				ParentID:       parent.ID,
				Name:           components[len(components)-1],
				ExpectedID:     current.ID,
				ExpectedSource: *current.Source,
				Source:         proposed,
			},
		)
		return err
	})
	if errors.Is(err, namespace.ErrConflict) || errors.Is(err, namespace.ErrNotFound) {
		return RebindResult{}, ErrConfirmationMismatch
	}
	if err != nil {
		return RebindResult{}, fmt.Errorf("rebind Projection %q: %w", request.Target, err)
	}
	result.Applied = true
	return result, nil
}

func (service *Service) createPrepared(
	ctx context.Context,
	request CreateRequest,
	candidate Candidate,
) (namespace.Node, error) {
	var created namespace.Node
	_, err := service.repository.Update(ctx, namespace.Operation{
		Name:  "projection.create",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     []string{candidate.Target},
			NodeKinds: []namespace.NodeKind{namespace.ProjectionFile},
		},
	}, func(transaction *namespace.Transaction) error {
		if err := transaction.ValidatePreparedLibrary(ctx, candidate.Library); err != nil {
			return err
		}
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

	source, err := service.projectionSource(sourcePath)
	if err != nil {
		return Candidate{}, err
	}

	return Candidate{
		Library:    library,
		Components: components,
		Target:     library.Name + ":/" + relativePath,
		Source:     source,
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

func parseLifecycleTarget(value string) (string, string, []string, error) {
	libraryName, relativePath, err := parseTarget(value)
	if err != nil {
		return "", "", nil, err
	}
	components, err := namespace.ValidateRelativePath(relativePath)
	if err != nil {
		return "", "", nil, err
	}
	relativePath = strings.Join(components, "/")
	return libraryName, relativePath, components, nil
}

func (service *Service) projectionAt(
	ctx context.Context,
	libraryName string,
	relativePath string,
) (namespace.Library, namespace.Node, error) {
	library, err := service.repository.Library(ctx, libraryName)
	if err != nil {
		return namespace.Library{}, namespace.Node{}, err
	}
	node, err := service.repository.Lookup(ctx, library.RootNodeID, relativePath)
	if errors.Is(err, namespace.ErrNotFound) {
		return namespace.Library{}, namespace.Node{}, fmt.Errorf(
			"%w: %s:/%s",
			ErrProjectionNotFound,
			libraryName,
			relativePath,
		)
	}
	if err != nil {
		return namespace.Library{}, namespace.Node{}, err
	}
	if node.Kind != namespace.ProjectionFile || node.Source == nil {
		return namespace.Library{}, namespace.Node{}, fmt.Errorf(
			"%w: %s:/%s",
			ErrProjectionNotFound,
			libraryName,
			relativePath,
		)
	}
	return library, node, nil
}

func projectionParent(
	ctx context.Context,
	transaction *namespace.Transaction,
	libraryRootID int64,
	components []string,
	attributes namespace.Attributes,
	actor namespace.Actor,
	create bool,
) (namespace.Node, error) {
	if len(components) == 1 {
		return transaction.Node(ctx, libraryRootID)
	}
	parentComponents := components[:len(components)-1]
	if create {
		return transaction.EnsureManagedDirectories(
			ctx,
			libraryRootID,
			parentComponents,
			attributes,
			actor,
		)
	}
	return transaction.Lookup(ctx, libraryRootID, strings.Join(parentComponents, "/"))
}

func (service *Service) projectionSource(path string) (namespace.ProjectionSource, error) {
	opened, err := sourcefile.Open(path)
	if err != nil {
		return namespace.ProjectionSource{}, classifySourceError(err)
	}
	metadata := opened.Metadata
	if err := opened.Close(); err != nil {
		return namespace.ProjectionSource{}, fmt.Errorf(
			"close Projection source %q: %w",
			metadata.Path,
			err,
		)
	}
	return namespace.ProjectionSource{
		Path:          metadata.Path,
		DeviceID:      metadata.DeviceID,
		Inode:         metadata.Inode,
		Size:          metadata.Size,
		AtimeNS:       metadata.AtimeNS,
		MtimeNS:       metadata.MtimeNS,
		SourceCtimeNS: metadata.CtimeNS,
		VerifiedAtNS:  service.now().UTC().UnixNano(),
	}, nil
}

func rebindConfirmationDigest(
	projectionID int64,
	target string,
	previous namespace.ProjectionSource,
	proposed namespace.ProjectionSource,
) (string, error) {
	type confirmationSource struct {
		Path     string `json:"path"`
		DeviceID int64  `json:"device_id"`
		Inode    int64  `json:"inode"`
		Size     int64  `json:"size"`
		MtimeNS  int64  `json:"mtime_ns"`
	}
	snapshot := func(source namespace.ProjectionSource) confirmationSource {
		return confirmationSource{
			Path:     source.Path,
			DeviceID: source.DeviceID,
			Inode:    source.Inode,
			Size:     source.Size,
			MtimeNS:  source.MtimeNS,
		}
	}
	encoded, err := json.Marshal(struct {
		Schema       string             `json:"schema"`
		ProjectionID int64              `json:"projection_id"`
		Target       string             `json:"target"`
		Previous     confirmationSource `json:"previous_source"`
		Proposed     confirmationSource `json:"proposed_source"`
	}{
		Schema:       "scrapefs.projection-rebind-confirmation/v1",
		ProjectionID: projectionID,
		Target:       target,
		Previous:     snapshot(previous),
		Proposed:     snapshot(proposed),
	})
	if err != nil {
		return "", fmt.Errorf("encode Projection rebind confirmation: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum), nil
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
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %w", ErrSourceNotFound, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w: %w", ErrSourcePermissionDenied, err)
	default:
		return err
	}
}
