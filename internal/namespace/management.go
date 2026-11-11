package namespace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zendext/scrape-fs/internal/database"
)

var ErrPreviewChanged = errors.New("the removal preview has changed")

type TreeEntry struct {
	Path string
	Node Node
}

type LibraryTree struct {
	Library Library
	Root    Node
	Path    string
	Entries []TreeEntry
}

func (tree LibraryTree) Digest() string {
	encoded, _ := json.Marshal(tree)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
}

func (repository *Repository) LibraryTree(ctx context.Context, name, path string, recursive bool) (LibraryTree, error) {
	var tree LibraryTree
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		var err error
		tree, err = readLibraryTree(ctx, connection, name, path, recursive)
		return err
	})
	return tree, err
}

func readLibraryTree(ctx context.Context, query nodeQuery, name, path string, recursive bool) (LibraryTree, error) {
	if err := validateName(name); err != nil {
		return LibraryTree{}, fmt.Errorf("%w: %v", ErrInvalidLibrary, err)
	}
	library, err := queryLibrary(ctx, query, "libraries.status = 'ACTIVE' AND libraries.name = ?", name)
	if errors.Is(err, ErrNotFound) {
		return LibraryTree{}, ErrLibraryNotFound
	}
	if err != nil {
		return LibraryTree{}, err
	}
	root, err := queryNode(ctx, query, "id = ?", library.RootNodeID)
	if err != nil {
		return LibraryTree{}, err
	}
	if path != "" {
		components, err := ValidateRelativePath(path)
		if err != nil {
			return LibraryTree{}, err
		}
		root, err = lookupComponents(ctx, query, library.RootNodeID, components)
		if err != nil {
			return LibraryTree{}, err
		}
	}
	if root.Kind != Directory && root.Kind != LibraryRoot {
		return LibraryTree{}, ErrNotDirectory
	}
	tree := LibraryTree{Library: library, Root: root, Path: path, Entries: []TreeEntry{}}
	pending := []TreeEntry{{Path: path, Node: root}}
	seen := map[int64]bool{root.ID: true}
	for len(pending) > 0 {
		parent := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		var cursor int64
		for {
			child, err := queryNode(ctx, query, "parent_id = ? AND id > ? ORDER BY id LIMIT 1", parent.Node.ID, cursor)
			if errors.Is(err, ErrNotFound) {
				break
			}
			if err != nil {
				return LibraryTree{}, err
			}
			if seen[child.ID] || child.Kind == GlobalRoot || child.Kind == LibraryRoot {
				return LibraryTree{}, ErrInvalidNode
			}
			seen[child.ID] = true
			cursor = child.ID
			childPath := child.Name
			if parent.Path != "" {
				childPath = parent.Path + "/" + child.Name
			}
			entry := TreeEntry{Path: childPath, Node: child}
			tree.Entries = append(tree.Entries, entry)
			if recursive && child.Kind == Directory {
				pending = append(pending, entry)
			}
		}
	}
	return tree, nil
}

func (repository *Repository) CreateDirectory(ctx context.Context, name, path, childName string, parentID int64, actor Actor) (Node, error) {
	if err := validateName(childName); err != nil {
		return Node{}, fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	var created Node
	_, err := repository.Update(ctx, Operation{
		Name: "directory.create", Actor: actor,
		Details: OperationDetails{Paths: []string{name + ":/" + strings.TrimPrefix(path+"/"+childName, "/")}, NodeKinds: []NodeKind{Directory}},
	}, func(transaction *Transaction) error {
		tree, err := readLibraryTree(ctx, transaction.transaction, name, path, false)
		if err != nil {
			return err
		}
		if parentID <= 0 || tree.Root.ID != parentID {
			return ErrPreviewChanged
		}
		created, err = transaction.CreateNode(ctx, CreateNode{
			ParentID: parentID, Name: childName, Kind: Directory, Creator: actor,
			Attributes: Attributes{UID: tree.Library.UID, GID: tree.Library.GID, Mode: tree.Library.DirectoryMode},
		})
		return err
	})
	return created, err
}

func (repository *Repository) MoveDirectory(ctx context.Context, name, path, target string, expectedID int64, actor Actor) (Node, error) {
	components, err := ValidateRelativePath(target)
	if err != nil {
		return Node{}, err
	}
	if path == "" {
		return Node{}, ErrProtected
	}
	var moved Node
	_, err = repository.Update(ctx, Operation{
		Name: "directory.move", Actor: actor,
		Details: OperationDetails{Paths: []string{name + ":/" + path, name + ":/" + target}, NodeKinds: []NodeKind{Directory, Directory}},
	}, func(transaction *Transaction) error {
		tree, err := readLibraryTree(ctx, transaction.transaction, name, path, false)
		if err != nil {
			return err
		}
		if tree.Root.Kind != Directory || tree.Root.ParentID == nil {
			return ErrProtected
		}
		if expectedID <= 0 || tree.Root.ID != expectedID {
			return ErrPreviewChanged
		}
		parent := tree.Library.RootNodeID
		if len(components) > 1 {
			node, err := lookupComponents(ctx, transaction.transaction, parent, components[:len(components)-1])
			if err != nil {
				return err
			}
			parent = node.ID
		}
		result, err := transaction.Rename(ctx, RenameNode{
			OldParentID: *tree.Root.ParentID, OldName: tree.Root.Name,
			NewParentID: parent, NewName: components[len(components)-1], NoReplace: true,
			ExpectedSourceID: expectedID, ExpectedSourceKind: Directory,
		})
		moved = result.Node
		return err
	})
	return moved, err
}

func (repository *Repository) RemoveTree(ctx context.Context, prepared LibraryTree, actor Actor) ([]int64, error) {
	var objectIDs []int64
	operation := "directory.remove"
	if prepared.Root.Kind == LibraryRoot {
		operation = "library.remove"
	}
	_, err := repository.TryUpdate(ctx, Operation{
		Name: operation, Actor: actor,
		Details: OperationDetails{Paths: []string{prepared.Library.Name + ":/" + prepared.Path}, NodeKinds: []NodeKind{prepared.Root.Kind}},
	}, func(transaction *Transaction) error {
		current, err := readLibraryTree(ctx, transaction.transaction, prepared.Library.Name, prepared.Path, true)
		if err != nil {
			return err
		}
		if current.Digest() != prepared.Digest() {
			return ErrPreviewChanged
		}
		_, objectIDs, err = transaction.removeLibraryTree(ctx, current)
		return err
	})
	return objectIDs, err
}

func (transaction *Transaction) removeLibraryTree(ctx context.Context, tree LibraryTree) (Library, []int64, error) {
	if tree.Root.Kind != Directory && tree.Root.Kind != LibraryRoot {
		return Library{}, nil, ErrProtected
	}
	now := transaction.now().UTC().UnixNano()
	retired := tree.Library
	if tree.Root.Kind == LibraryRoot {
		result, err := transaction.transaction.ExecContext(ctx, `
			UPDATE libraries SET status = 'RETIRED', root_node_id = NULL, updated_at = ?, retired_at = ?
			WHERE id = ? AND status = 'ACTIVE' AND root_node_id = ?
		`, now, now, tree.Library.ID, tree.Root.ID)
		if err != nil {
			return Library{}, nil, classifyLibraryMutationError("retire Library", err)
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return Library{}, nil, ErrPreviewChanged
		}
		retired.Status, retired.RootNodeID, retired.UpdatedAt = LibraryRetired, 0, now
	}
	objectIDs := []int64{}
	for index := len(tree.Entries) - 1; index >= 0; index-- {
		node := tree.Entries[index].Node
		if _, err := transaction.removeNodeWithoutTouch(ctx, node); err != nil {
			return Library{}, nil, err
		}
		if node.ObjectID != nil {
			objectIDs = append(objectIDs, *node.ObjectID)
		}
	}
	if tree.Root.Kind == LibraryRoot {
		if _, err := transaction.transaction.ExecContext(ctx, "DELETE FROM nodes WHERE id = ? AND kind = 'LIBRARY_ROOT'", tree.Root.ID); err != nil {
			return Library{}, nil, classifyMutationError("remove LibraryRoot", err)
		}
	} else if _, err := transaction.removeNodeWithoutTouch(ctx, tree.Root); err != nil {
		return Library{}, nil, err
	}
	if tree.Root.ParentID == nil {
		return Library{}, nil, ErrInvalidNode
	}
	if err := transaction.touchDirectory(ctx, *tree.Root.ParentID, now); err != nil {
		return Library{}, nil, err
	}
	transaction.changed = true
	return retired, objectIDs, nil
}
