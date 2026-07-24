package fuse

import (
	"context"
	"testing"

	"github.com/zendext/scrape-fs/internal/namespace"
)

func TestMountedInvalidateDropsStaleControlEntriesAndAttributes(t *testing.T) {
	root := &node{
		record:   testNode(1, 0, "", namespace.GlobalRoot),
		registry: newInodeRegistry(),
	}
	initializeNodeFS(root)
	root.registry.Store(root.record.ID, &root.Inode)

	childOperations := &node{
		record:   testNode(2, 1, "old", namespace.LibraryRoot),
		registry: root.registry,
	}
	child := root.NewInode(
		context.Background(),
		childOperations,
		stableAttr(childOperations.record),
	)
	root.AddChild("old", child, true)
	root.registry.Store(childOperations.record.ID, child)
	mounted := &Mounted{registry: root.registry}

	errs := mounted.Invalidate(namespace.Commit{
		Actor: namespace.Actor{Kind: namespace.ActorControl},
	}, namespace.ChangeSet{
		Entries: []namespace.EntryChange{
			{ParentID: 1, NodeID: 2, Name: "old", Removed: true},
			{ParentID: 1, NodeID: 2, Name: "new"},
		},
		InodeIDs: []int64{1, 2},
	})
	if root.GetChild("old") != nil {
		t.Fatal("stale child remains in inode tree")
	}
	if len(errs) == 0 {
		t.Fatal("Invalidate without mounted server reported no notification failures")
	}
}

func TestMountedInvalidateLeavesFuseManagedEntryTreeToBridge(t *testing.T) {
	root := &node{
		record:   testNode(1, 0, "", namespace.GlobalRoot),
		registry: newInodeRegistry(),
	}
	initializeNodeFS(root)
	root.registry.Store(root.record.ID, &root.Inode)
	childOperations := &node{
		record:   testNode(2, 1, "child", namespace.LibraryRoot),
		registry: root.registry,
	}
	child := root.NewInode(
		context.Background(),
		childOperations,
		stableAttr(childOperations.record),
	)
	root.AddChild("child", child, true)
	root.registry.Store(childOperations.record.ID, child)
	mounted := &Mounted{registry: root.registry}

	mounted.Invalidate(namespace.Commit{
		Actor: namespace.Actor{Kind: namespace.ActorFuse},
	}, namespace.ChangeSet{
		Entries: []namespace.EntryChange{
			{ParentID: 1, NodeID: 2, Name: "child", Removed: true},
		},
		InodeIDs: []int64{1, 2},
	})
	if root.GetChild("child") == nil {
		t.Fatal("FUSE commit removed child before bridge completed mutation")
	}
}

func TestNodeForgetRemovesOnlyItsCurrentRegistryEntry(t *testing.T) {
	registry := newInodeRegistry()
	forgotten := &node{
		record:   testNode(2, 1, "old", namespace.Directory),
		registry: registry,
	}
	replacement := &node{
		record:   testNode(2, 1, "new", namespace.Directory),
		registry: registry,
	}

	registry.Store(2, &forgotten.Inode)
	forgotten.OnForget()
	if got := registry.Load(2); got != nil {
		t.Fatalf("registry after forget = %p, want nil", got)
	}

	registry.Store(2, &forgotten.Inode)
	registry.Store(2, &replacement.Inode)
	forgotten.OnForget()
	if got := registry.Load(2); got != &replacement.Inode {
		t.Fatalf("registry after stale forget = %p, want replacement %p", got, &replacement.Inode)
	}
}
