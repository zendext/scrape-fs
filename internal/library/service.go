package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/zendext/scrape-fs/internal/namespace"
)

var ErrProfileNotFound = namespace.ErrProfileNotFound
var ErrAlreadyExists = errors.New("library already exists")

type ProfileIdentifier struct {
	Name    string
	Version int64
}

type Modes struct {
	Root       fs.FileMode
	Directory  fs.FileMode
	Projection fs.FileMode
	Overlay    fs.FileMode
}

var DefaultModes = Modes{
	Root:       0o775,
	Directory:  0o775,
	Projection: 0o444,
	Overlay:    0o664,
}

type CreateRequest struct {
	Name    string
	Profile ProfileIdentifier
	Actor   namespace.Actor
	UID     uint32
	GID     uint32
	Modes   Modes
}

type Service struct {
	repository *namespace.Repository
}

func NewService(repository *namespace.Repository) *Service {
	return &Service{repository: repository}
}

func (service *Service) Create(
	ctx context.Context,
	request CreateRequest,
) (namespace.Library, error) {
	create := namespace.CreateLibrary{
		Name:           request.Name,
		ProfileName:    request.Profile.Name,
		ProfileVersion: request.Profile.Version,
		Creator:        request.Actor,
		UID:            request.UID,
		GID:            request.GID,
		RootMode:       request.Modes.Root,
		DirectoryMode:  request.Modes.Directory,
		ProjectionMode: request.Modes.Projection,
		OverlayMode:    request.Modes.Overlay,
	}
	if err := create.Validate(); err != nil {
		return namespace.Library{}, err
	}

	var created namespace.Library
	_, err := service.repository.Update(ctx, namespace.Operation{
		Name:  "library.create",
		Actor: request.Actor,
		Details: namespace.OperationDetails{
			Paths:     []string{request.Name},
			NodeKinds: []namespace.NodeKind{namespace.LibraryRoot},
		},
	}, func(transaction *namespace.Transaction) error {
		var err error
		created, err = transaction.CreateLibrary(ctx, create)
		return err
	})
	if err != nil {
		if errors.Is(err, namespace.ErrConflict) {
			return namespace.Library{}, fmt.Errorf("%w: %w", ErrAlreadyExists, err)
		}
		return namespace.Library{}, err
	}
	return created, nil
}

func (service *Service) Get(ctx context.Context, name string) (namespace.Library, error) {
	return service.repository.Library(ctx, name)
}

func (service *Service) List(ctx context.Context) ([]namespace.Library, error) {
	return service.repository.Libraries(ctx)
}
