package library

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/zendext/scrape-fs/internal/namespace"
	"golang.org/x/sys/unix"
)

type Entry struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	Modified  int64  `json:"modified"`
	Source    string `json:"source,omitempty"`
	CanRebind bool   `json:"can_rebind"`
	Available bool   `json:"available"`
}

type BrowseResult struct {
	Schema  string  `json:"schema"`
	Library string  `json:"library"`
	Path    string  `json:"path"`
	ID      int64   `json:"id"`
	Entries []Entry `json:"entries"`
}

type RemovalPreview struct {
	Schema      string `json:"schema"`
	Library     string `json:"library"`
	Path        string `json:"path"`
	ID          int64  `json:"id"`
	Directories int    `json:"directories"`
	Mappings    int    `json:"mappings"`
	Files       int    `json:"files"`
	Unavailable int    `json:"unavailable"`
	Digest      string `json:"digest"`
}

type objectStamp struct {
	NodeID      int64
	Size        int64
	Modified    int64
	Changed     int64
	Unavailable bool
}

type objectStatter interface {
	StatObject(context.Context, int64) (unix.Stat_t, error)
}

func (service *Service) Browse(ctx context.Context, name, path string) (BrowseResult, error) {
	tree, err := service.repository.LibraryTree(ctx, name, path, false)
	if err != nil {
		return BrowseResult{}, err
	}
	result := BrowseResult{Schema: "scrapefs.directory/v1", Library: name, Path: path, ID: tree.Root.ID, Entries: []Entry{}}
	for _, record := range tree.Entries {
		node := record.Node
		entry := Entry{ID: node.ID, Name: node.Name, Path: record.Path, Modified: node.MtimeNS / 1e6, Available: true}
		switch node.Kind {
		case namespace.Directory:
			entry.Kind = "directory"
		case namespace.ProjectionFile:
			entry.Kind, entry.Source, entry.Size = "mapping", node.Source.Path, node.Source.Size
			entry.Modified = node.Source.MtimeNS / 1e6
			entry.CanRebind = node.OriginScrapeID == nil
		case namespace.OverlayFile:
			entry.Kind = "file"
			stamp := service.objectStamp(ctx, node)
			entry.Available, entry.Size, entry.Modified = !stamp.Unavailable, stamp.Size, stamp.Modified/1e6
		default:
			return BrowseResult{}, namespace.ErrInvalidNode
		}
		result.Entries = append(result.Entries, entry)
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		left, right := result.Entries[i], result.Entries[j]
		if (left.Kind == "directory") != (right.Kind == "directory") {
			return left.Kind == "directory"
		}
		return left.Name < right.Name
	})
	return result, nil
}

func (service *Service) CreateDirectory(ctx context.Context, name, path, child string, parentID int64, actor namespace.Actor) error {
	_, err := service.repository.CreateDirectory(ctx, name, path, child, parentID, actor)
	return err
}

func (service *Service) MoveDirectory(ctx context.Context, name, path, target string, id int64, actor namespace.Actor) error {
	_, err := service.repository.MoveDirectory(ctx, name, path, target, id, actor)
	return err
}

func (service *Service) PreviewRemoval(ctx context.Context, name, path string) (RemovalPreview, error) {
	preview, _, err := service.prepareRemoval(ctx, name, path)
	return preview, err
}

func (service *Service) prepareRemoval(ctx context.Context, name, path string) (RemovalPreview, namespace.LibraryTree, error) {
	tree, err := service.repository.LibraryTree(ctx, name, path, true)
	if err != nil {
		return RemovalPreview{}, namespace.LibraryTree{}, err
	}
	preview := RemovalPreview{Schema: "scrapefs.removal-preview/v1", Library: name, Path: path, ID: tree.Root.ID}
	stamps := []objectStamp{}
	for _, record := range tree.Entries {
		switch record.Node.Kind {
		case namespace.Directory:
			preview.Directories++
		case namespace.ProjectionFile:
			preview.Mappings++
		case namespace.OverlayFile:
			preview.Files++
			stamp := service.objectStamp(ctx, record.Node)
			stamps = append(stamps, stamp)
			if stamp.Unavailable {
				preview.Unavailable++
			}
		default:
			return RemovalPreview{}, namespace.LibraryTree{}, namespace.ErrInvalidNode
		}
	}
	encoded, err := json.Marshal(struct {
		Tree    string
		Objects []objectStamp
	}{tree.Digest(), stamps})
	if err != nil {
		return RemovalPreview{}, namespace.LibraryTree{}, err
	}
	preview.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
	return preview, tree, nil
}

func (service *Service) RemoveTree(ctx context.Context, name, path, digest string, actor namespace.Actor) error {
	preview, tree, err := service.prepareRemoval(ctx, name, path)
	if err != nil {
		return err
	}
	if digest == "" || preview.Digest != digest {
		return namespace.ErrPreviewChanged
	}
	objectIDs, err := service.repository.RemoveTree(ctx, tree, actor)
	if err != nil {
		return err
	}
	if service.objectReclaimer != nil {
		for _, objectID := range objectIDs {
			_ = service.objectReclaimer.DeleteObject(objectID)
		}
	}
	return nil
}

func (service *Service) objectStamp(ctx context.Context, node namespace.Node) objectStamp {
	stamp := objectStamp{NodeID: node.ID, Unavailable: true}
	statter, ok := service.objectReclaimer.(objectStatter)
	if !ok || node.ObjectID == nil {
		return stamp
	}
	stat, err := statter.StatObject(ctx, *node.ObjectID)
	if err != nil {
		return stamp
	}
	return objectStamp{NodeID: node.ID, Size: stat.Size, Modified: stat.Mtim.Nano(), Changed: stat.Ctim.Nano()}
}
