package namespace

import (
	"encoding/json"
	"errors"
	"io/fs"
)

var (
	ErrConflict         = errors.New("namespace conflict")
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
	Details  json.RawMessage
}

type Attributes struct {
	Mode fs.FileMode
	UID  uint32
	GID  uint32
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
