package namespace

import (
	"errors"
	"io/fs"
)

var (
	ErrConflict         = errors.New("namespace conflict")
	ErrInvalidLibrary   = errors.New("invalid library")
	ErrLibraryNotFound  = errors.New("library not found")
	ErrInvalidObject    = errors.New("invalid backing object")
	ErrProfileNotFound  = errors.New("profile not found")
	ErrInvalidNode      = errors.New("invalid namespace node")
	ErrInvalidOperation = errors.New("invalid namespace operation")
	ErrInvalidPath      = errors.New("invalid relative namespace path")
	ErrInvalidRoot      = errors.New("invalid library root")
	ErrNotFound         = errors.New("namespace node not found")
)

type NodeKind string

const (
	GlobalRoot     NodeKind = "GLOBAL_ROOT"
	LibraryRoot    NodeKind = "LIBRARY_ROOT"
	Directory      NodeKind = "DIRECTORY"
	ProjectionFile NodeKind = "PROJECTION_FILE"
	OverlayFile    NodeKind = "OVERLAY_FILE"
)

func (kind NodeKind) valid() bool {
	switch kind {
	case GlobalRoot, LibraryRoot, Directory, ProjectionFile, OverlayFile:
		return true
	default:
		return false
	}
}

type ActorKind string

const (
	ActorControl ActorKind = "control"
	ActorAgent   ActorKind = "agent"
	ActorFuse    ActorKind = "fuse"
	ActorSystem  ActorKind = "system"
)

type Actor struct {
	Kind ActorKind
	ID   *int64
}

func (actor Actor) valid() bool {
	switch actor.Kind {
	case ActorSystem:
		return actor.ID == nil
	case ActorControl, ActorAgent, ActorFuse:
		return actor.ID != nil && *actor.ID >= 0
	default:
		return false
	}
}

type Operation struct {
	Name     string
	Actor    Actor
	ScrapeID *int64
	Details  OperationDetails
}

type OperationDetails struct {
	Paths     []string   `json:"paths"`
	NodeKinds []NodeKind `json:"node_kinds"`
}

type Attributes struct {
	Mode fs.FileMode
	UID  uint32
	GID  uint32
}

type LibraryStatus string

const (
	LibraryActive  LibraryStatus = "ACTIVE"
	LibraryRetired LibraryStatus = "RETIRED"
)

type Library struct {
	ID             int64
	Name           string
	Status         LibraryStatus
	RootNodeID     int64
	ProfileName    string
	ProfileVersion int64
	UID            uint32
	GID            uint32
	RootMode       fs.FileMode
	DirectoryMode  fs.FileMode
	ProjectionMode fs.FileMode
	OverlayMode    fs.FileMode
	CreatedAt      int64
	UpdatedAt      int64
}

type CreateLibrary struct {
	Name           string
	ProfileName    string
	ProfileVersion int64
	Creator        Actor
	UID            uint32
	GID            uint32
	RootMode       fs.FileMode
	DirectoryMode  fs.FileMode
	ProjectionMode fs.FileMode
	OverlayMode    fs.FileMode
}

func (request CreateLibrary) Validate() error {
	return validateCreateLibrary(request)
}

type ProjectionSource struct {
	Path          string
	DeviceID      int64
	Inode         int64
	Size          int64
	AtimeNS       int64
	MtimeNS       int64
	SourceCtimeNS int64
	VerifiedAtNS  int64
}

type CreateNode struct {
	ParentID         int64
	Name             string
	Kind             NodeKind
	Source           *ProjectionSource
	ObjectID         *int64
	OriginScrapeID   *int64
	Creator          Actor
	ManagedDirectory bool
	Attributes       Attributes
}

type CreateOverlay struct {
	ParentID   int64
	Name       string
	Creator    Actor
	Attributes Attributes
}

type Node struct {
	ID               int64
	ParentID         *int64
	Name             string
	Kind             NodeKind
	Source           *ProjectionSource
	ObjectID         *int64
	OriginScrapeID   *int64
	Creator          Actor
	ManagedDirectory bool
	Attributes       Attributes
	AtimeNS          int64
	MtimeNS          int64
	VirtualCtimeNS   int64
	CreatedAt        int64
	UpdatedAt        int64
}

type Projection struct {
	LibraryID   int64
	LibraryName string
	Target      string
	Node        Node
}

type ProjectionBatchStatus string

const (
	ProjectionBatchApplied ProjectionBatchStatus = "applied"
	ProjectionBatchUndone  ProjectionBatchStatus = "undone"
)

type ProjectionBatch struct {
	ID                   int64
	LibraryID            int64
	LibraryName          string
	LibraryRootID        int64
	LibraryRootAvailable bool
	ProfileName          string
	ProfileVersion       int64
	Digest               string
	Status               ProjectionBatchStatus
	AppliedByUID         uint32
	AppliedByGID         uint32
	AppliedAt            int64
	UndoneByUID          *uint32
	UndoneByGID          *uint32
	UndoneAt             *int64
}

type ProjectionBatchEntry struct {
	Position         int64
	Source           ProjectionSource
	Target           string
	ProjectionNodeID *int64
}

type ProjectionBatchOperation struct {
	ID               int64
	Name             string
	Actor            Actor
	NamespaceVersion int64
	Details          OperationDetails
	CreatedAt        int64
}

type CreateProjectionBatch struct {
	LibraryID      int64
	ProfileName    string
	ProfileVersion int64
	Digest         string
	AppliedByUID   uint32
	AppliedByGID   uint32
	AppliedAt      int64
}

type CreateProjectionBatchEntry struct {
	BatchID          int64
	Position         int64
	Source           ProjectionSource
	Target           string
	ProjectionNodeID int64
}
