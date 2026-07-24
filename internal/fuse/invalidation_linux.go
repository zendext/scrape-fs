package fuse

import (
	"fmt"
	"sync"
	"syscall"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/zendext/scrape-fs/internal/namespace"
)

type inodeRegistry struct {
	mu     sync.RWMutex
	inodes map[int64]*gofs.Inode
}

func newInodeRegistry() *inodeRegistry {
	return &inodeRegistry{inodes: make(map[int64]*gofs.Inode)}
}

func (registry *inodeRegistry) Store(id int64, inode *gofs.Inode) {
	if registry == nil || id <= 0 || inode == nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.inodes[id] = inode
}

func (registry *inodeRegistry) Load(id int64) *gofs.Inode {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.inodes[id]
}

func (registry *inodeRegistry) Delete(id int64, inode *gofs.Inode) {
	if registry == nil || id <= 0 || inode == nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.inodes[id] == inode {
		delete(registry.inodes, id)
	}
}

func (current *node) OnForget() {
	current.registry.Delete(current.record.ID, &current.Inode)
}

func (mounted *Mounted) Invalidate(
	commit namespace.Commit,
	changes namespace.ChangeSet,
) []error {
	if mounted == nil || mounted.registry == nil {
		return nil
	}
	var failures []error
	for _, id := range changes.InodeIDs {
		inode := mounted.registry.Load(id)
		if inode == nil {
			continue
		}
		if errno := inode.NotifyContent(0, 0); errno != 0 {
			failures = append(failures, invalidationError(
				"inode",
				id,
				"",
				errno,
			))
		}
	}
	if commit.Actor.Kind == namespace.ActorFuse {
		return failures
	}
	for _, entry := range changes.Entries {
		parent := mounted.registry.Load(entry.ParentID)
		if parent == nil {
			continue
		}
		if entry.Removed {
			child := parent.GetChild(entry.Name)
			if child != nil {
				if errno := parent.NotifyDelete(entry.Name, child); errno != 0 {
					failures = append(failures, invalidationError(
						"entry",
						entry.ParentID,
						entry.Name,
						errno,
					))
				}
				parent.RmChild(entry.Name)
				continue
			}
		} else if parent.GetChild(entry.Name) != nil {
			parent.RmChild(entry.Name)
		}
		if errno := parent.NotifyEntry(entry.Name); errno != 0 {
			failures = append(failures, invalidationError(
				"entry",
				entry.ParentID,
				entry.Name,
				errno,
			))
		}
	}
	return failures
}

func invalidationError(
	kind string,
	inodeID int64,
	name string,
	errno syscall.Errno,
) error {
	if name == "" {
		return fmt.Errorf("invalidate FUSE %s %d: %w", kind, inodeID, errno)
	}
	return fmt.Errorf(
		"invalidate FUSE %s %d/%q: %w",
		kind,
		inodeID,
		name,
		errno,
	)
}
