package namespace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/zendext/scrape-fs/internal/database"
)

type Commit struct {
	Operation string
	Actor     Actor
}

type CommitListener func(Commit)

type EntryChange struct {
	ParentID int64
	NodeID   int64
	Name     string
	Removed  bool
}

type ChangeSet struct {
	Entries  []EntryChange
	InodeIDs []int64
}

type IndexStats struct {
	Libraries int
	Nodes     int
	Objects   int
}

type IndexInspection struct {
	Index      IndexStats
	Repository IndexStats
	Diverged   bool
}

type RepositoryInspection struct {
	NamespaceVersion int64
	ActiveLibraries  int
	RetiredLibraries int
	Nodes            int
	Projections      int
	OverlayFiles     int
	Libraries        []Library
}

type Index struct {
	repository *Repository
	mu         sync.RWMutex
	libraries  map[int64]Library
	nodes      map[int64]Node
	objects    map[int64][]int64
}

func NewIndex(repository *Repository) *Index {
	return &Index{repository: repository}
}

func (index *Index) Rebuild(ctx context.Context) (ChangeSet, error) {
	if index == nil || index.repository == nil {
		return ChangeSet{}, errors.New("namespace index repository is required")
	}
	snapshot, err := index.repository.snapshot(ctx)
	if err != nil {
		return ChangeSet{}, err
	}
	libraries, nodes, objects, err := buildIndex(snapshot)
	if err != nil {
		return ChangeSet{}, err
	}

	index.mu.Lock()
	changes := diffNodes(index.nodes, nodes)
	index.libraries = libraries
	index.nodes = nodes
	index.objects = objects
	index.mu.Unlock()
	return changes, nil
}

func (index *Index) Stats() IndexStats {
	if index == nil {
		return IndexStats{}
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	return IndexStats{
		Libraries: len(index.libraries),
		Nodes:     len(index.nodes),
		Objects:   len(index.objects),
	}
}

func (index *Index) Inspect(ctx context.Context) (IndexInspection, error) {
	if index == nil || index.repository == nil {
		return IndexInspection{}, errors.New("namespace index repository is required")
	}
	for {
		inspection, generation, err := index.inspectOnce(ctx)
		if err != nil || !inspection.Diverged {
			return inspection, err
		}
		currentGeneration, notification := index.repository.commitListenerState()
		if currentGeneration != generation {
			continue
		}
		if notification == nil {
			return inspection, nil
		}
		select {
		case <-ctx.Done():
			return IndexInspection{}, ctx.Err()
		case <-notification:
		}
	}
}

func (index *Index) inspectOnce(
	ctx context.Context,
) (IndexInspection, uint64, error) {
	snapshot, err := index.repository.snapshot(ctx)
	if err != nil {
		return IndexInspection{}, 0, err
	}
	libraries, nodes, objects, err := buildIndex(snapshot)
	if err != nil {
		return IndexInspection{}, 0, err
	}

	index.mu.RLock()
	defer index.mu.RUnlock()
	return IndexInspection{
		Index:      indexStats(index.libraries, index.nodes, index.objects),
		Repository: indexStats(libraries, nodes, objects),
		Diverged: !equalLibraries(index.libraries, libraries) ||
			!equalNodes(index.nodes, nodes) ||
			!equalObjects(index.objects, objects),
	}, snapshot.listenerGeneration, nil
}

func (repository *Repository) Inspect(ctx context.Context) (RepositoryInspection, error) {
	var inspection RepositoryInspection
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		version, err := namespaceVersion(ctx, connection)
		if err != nil {
			return err
		}
		inspection.NamespaceVersion = version

		var snapshot repositorySnapshot
		var libraryCursorID int64
		for {
			var libraryID int64
			err := connection.QueryRowContext(ctx, `
				SELECT id
				FROM libraries
				WHERE id > ?
				ORDER BY id
				LIMIT 1
			`, libraryCursorID).Scan(&libraryID)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return fmt.Errorf("list diagnostic Libraries: %w", err)
			}
			library, err := queryLibrary(
				ctx,
				connection,
				"libraries.id = ?",
				libraryID,
			)
			if err != nil {
				return fmt.Errorf("inspect Library %d: %w", libraryID, err)
			}
			switch library.Status {
			case LibraryActive:
				inspection.ActiveLibraries++
				snapshot.libraries = append(snapshot.libraries, library)
				inspection.Libraries = append(inspection.Libraries, library)
			case LibraryRetired:
				inspection.RetiredLibraries++
			}
			libraryCursorID = libraryID
		}

		var nodeCursorID int64
		for {
			node, err := queryNode(ctx, connection, `
				id > ?
				ORDER BY id
				LIMIT 1
			`, nodeCursorID)
			if errors.Is(err, ErrNotFound) {
				break
			}
			if err != nil {
				return fmt.Errorf("inspect namespace nodes: %w", err)
			}
			snapshot.nodes = append(snapshot.nodes, node)
			inspection.Nodes++
			switch node.Kind {
			case ProjectionFile:
				inspection.Projections++
			case OverlayFile:
				inspection.OverlayFiles++
			}
			nodeCursorID = node.ID
		}
		_, _, _, err = buildIndex(snapshot)
		return err
	})
	if err != nil {
		return RepositoryInspection{}, err
	}
	return inspection, nil
}

type repositorySnapshot struct {
	libraries          []Library
	nodes              []Node
	listenerGeneration uint64
}

func indexStats(
	libraries map[int64]Library,
	nodes map[int64]Node,
	objects map[int64][]int64,
) IndexStats {
	return IndexStats{
		Libraries: len(libraries),
		Nodes:     len(nodes),
		Objects:   len(objects),
	}
}

func equalLibraries(left, right map[int64]Library) bool {
	if len(left) != len(right) {
		return false
	}
	for id, library := range left {
		if other, exists := right[id]; !exists || other != library {
			return false
		}
	}
	return true
}

func equalNodes(left, right map[int64]Node) bool {
	if len(left) != len(right) {
		return false
	}
	for id, node := range left {
		other, exists := right[id]
		if !exists || !equalNode(node, other) {
			return false
		}
	}
	return true
}

func equalObjects(left, right map[int64][]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for id, nodeIDs := range left {
		other, exists := right[id]
		if !exists || len(other) != len(nodeIDs) {
			return false
		}
		for index := range nodeIDs {
			if nodeIDs[index] != other[index] {
				return false
			}
		}
	}
	return true
}

func (repository *Repository) snapshot(ctx context.Context) (repositorySnapshot, error) {
	var snapshot repositorySnapshot
	err := repository.useConnection(ctx, func(connection *database.Connection) error {
		snapshot.listenerGeneration, _ = repository.commitListenerState()

		var libraryCursorName string
		var libraryCursorID int64
		for {
			library, err := queryLibrary(ctx, connection, `
				libraries.status = 'ACTIVE'
				AND (
					libraries.name > ?
					OR (libraries.name = ? AND libraries.id > ?)
				)
				ORDER BY libraries.name, libraries.id
				LIMIT 1
			`, libraryCursorName, libraryCursorName, libraryCursorID)
			if errors.Is(err, ErrNotFound) {
				break
			}
			if err != nil {
				return fmt.Errorf("snapshot Libraries: %w", err)
			}
			snapshot.libraries = append(snapshot.libraries, library)
			libraryCursorName = library.Name
			libraryCursorID = library.ID
		}

		var nodeCursorID int64
		for {
			node, err := queryNode(ctx, connection, `
				id > ?
				ORDER BY id
				LIMIT 1
			`, nodeCursorID)
			if errors.Is(err, ErrNotFound) {
				break
			}
			if err != nil {
				return fmt.Errorf("snapshot nodes: %w", err)
			}
			snapshot.nodes = append(snapshot.nodes, node)
			nodeCursorID = node.ID
		}
		return nil
	})
	if err != nil {
		return repositorySnapshot{}, err
	}
	return snapshot, nil
}

func buildIndex(
	snapshot repositorySnapshot,
) (map[int64]Library, map[int64]Node, map[int64][]int64, error) {
	libraries := make(map[int64]Library, len(snapshot.libraries))
	rootLibraries := make(map[int64]Library, len(snapshot.libraries))
	for _, library := range snapshot.libraries {
		if library.ID <= 0 || library.Status != LibraryActive || library.RootNodeID <= 0 {
			return nil, nil, nil, fmt.Errorf("%w: invalid active Library in index", ErrInvalidLibrary)
		}
		if _, duplicate := libraries[library.ID]; duplicate {
			return nil, nil, nil, fmt.Errorf("%w: duplicate Library %d", ErrInvalidLibrary, library.ID)
		}
		if existing, duplicate := rootLibraries[library.RootNodeID]; duplicate {
			return nil, nil, nil, fmt.Errorf(
				"%w: Libraries %d and %d share root node %d",
				ErrInvalidLibrary,
				existing.ID,
				library.ID,
				library.RootNodeID,
			)
		}
		libraries[library.ID] = library
		rootLibraries[library.RootNodeID] = library
	}

	nodes := make(map[int64]Node, len(snapshot.nodes))
	objects := make(map[int64][]int64)
	for _, node := range snapshot.nodes {
		if _, duplicate := nodes[node.ID]; duplicate {
			return nil, nil, nil, fmt.Errorf("%w: duplicate node %d", ErrInvalidNode, node.ID)
		}
		nodes[node.ID] = node
		if node.ObjectID != nil {
			objects[*node.ObjectID] = append(objects[*node.ObjectID], node.ID)
		}
	}
	root, exists := nodes[1]
	if !exists || root.Kind != GlobalRoot || root.ParentID != nil {
		return nil, nil, nil, fmt.Errorf("%w: missing GlobalRoot", ErrInvalidRoot)
	}
	for _, library := range snapshot.libraries {
		rootNode, exists := nodes[library.RootNodeID]
		if !exists ||
			rootNode.Kind != LibraryRoot ||
			rootNode.ParentID == nil ||
			*rootNode.ParentID != root.ID ||
			rootNode.Name != library.Name {
			return nil, nil, nil, fmt.Errorf(
				"%w: Library %d root relationship is invalid",
				ErrInvalidLibrary,
				library.ID,
			)
		}
	}
	for _, node := range snapshot.nodes {
		id := node.ID
		if id == root.ID {
			continue
		}
		if node.ParentID == nil {
			return nil, nil, nil, fmt.Errorf("%w: node %d has no parent", ErrInvalidNode, id)
		}
		parent, exists := nodes[*node.ParentID]
		if !exists {
			return nil, nil, nil, fmt.Errorf(
				"%w: node %d parent %d is missing",
				ErrInvalidNode,
				id,
				*node.ParentID,
			)
		}
		if node.Kind == LibraryRoot {
			if _, active := rootLibraries[id]; !active {
				return nil, nil, nil, fmt.Errorf(
					"%w: LibraryRoot node %d is not bound to an active Library",
					ErrInvalidNode,
					id,
				)
			}
		} else if parent.Kind != LibraryRoot && parent.Kind != Directory {
			return nil, nil, nil, fmt.Errorf(
				"%w: node %d parent %d is %s, want LibraryRoot or Directory",
				ErrInvalidNode,
				id,
				parent.ID,
				parent.Kind,
			)
		}
		if err := validateIndexedAncestry(id, nodes, rootLibraries); err != nil {
			return nil, nil, nil, err
		}
	}
	for objectID := range objects {
		sort.Slice(objects[objectID], func(left, right int) bool {
			return objects[objectID][left] < objects[objectID][right]
		})
	}
	return libraries, nodes, objects, nil
}

func validateIndexedAncestry(
	nodeID int64,
	nodes map[int64]Node,
	rootLibraries map[int64]Library,
) error {
	seen := make(map[int64]struct{})
	currentID := nodeID
	for {
		if _, duplicate := seen[currentID]; duplicate {
			return fmt.Errorf("%w: node %d ancestry contains a cycle", ErrInvalidNode, nodeID)
		}
		seen[currentID] = struct{}{}
		if _, isLibraryRoot := rootLibraries[currentID]; isLibraryRoot {
			return nil
		}
		current := nodes[currentID]
		if current.ParentID == nil || *current.ParentID == 1 {
			return fmt.Errorf(
				"%w: node %d is outside an active Library",
				ErrInvalidNode,
				nodeID,
			)
		}
		currentID = *current.ParentID
	}
}

func diffNodes(oldNodes, newNodes map[int64]Node) ChangeSet {
	var changes ChangeSet
	inodes := make(map[int64]struct{})
	for id, oldNode := range oldNodes {
		newNode, exists := newNodes[id]
		if !exists {
			appendEntryChange(&changes, oldNode, true)
			inodes[id] = struct{}{}
			continue
		}
		if oldNode.ParentID == nil ||
			newNode.ParentID == nil ||
			*oldNode.ParentID != *newNode.ParentID ||
			oldNode.Name != newNode.Name {
			appendEntryChange(&changes, oldNode, true)
			appendEntryChange(&changes, newNode, false)
		}
		if !equalNode(oldNode, newNode) {
			inodes[id] = struct{}{}
		}
	}
	for id, newNode := range newNodes {
		if _, exists := oldNodes[id]; exists {
			continue
		}
		appendEntryChange(&changes, newNode, false)
		inodes[id] = struct{}{}
	}
	sort.Slice(changes.Entries, func(left, right int) bool {
		if changes.Entries[left].ParentID != changes.Entries[right].ParentID {
			return changes.Entries[left].ParentID < changes.Entries[right].ParentID
		}
		if changes.Entries[left].NodeID != changes.Entries[right].NodeID {
			return changes.Entries[left].NodeID < changes.Entries[right].NodeID
		}
		if changes.Entries[left].Removed != changes.Entries[right].Removed {
			return changes.Entries[left].Removed
		}
		return changes.Entries[left].Name < changes.Entries[right].Name
	})
	for id := range inodes {
		changes.InodeIDs = append(changes.InodeIDs, id)
	}
	sort.Slice(changes.InodeIDs, func(left, right int) bool {
		return changes.InodeIDs[left] < changes.InodeIDs[right]
	})
	return changes
}

func appendEntryChange(changes *ChangeSet, node Node, removed bool) {
	if node.ParentID == nil {
		return
	}
	changes.Entries = append(changes.Entries, EntryChange{
		ParentID: *node.ParentID,
		NodeID:   node.ID,
		Name:     node.Name,
		Removed:  removed,
	})
}

func equalNode(left, right Node) bool {
	return left.ID == right.ID &&
		equalInt64Pointer(left.ParentID, right.ParentID) &&
		left.Name == right.Name &&
		left.Kind == right.Kind &&
		equalProjectionSource(left.Source, right.Source) &&
		equalInt64Pointer(left.ObjectID, right.ObjectID) &&
		equalInt64Pointer(left.OriginScrapeID, right.OriginScrapeID) &&
		equalActor(left.Creator, right.Creator) &&
		left.ManagedDirectory == right.ManagedDirectory &&
		left.Attributes == right.Attributes &&
		left.AtimeNS == right.AtimeNS &&
		left.MtimeNS == right.MtimeNS &&
		left.VirtualCtimeNS == right.VirtualCtimeNS &&
		left.CreatedAt == right.CreatedAt &&
		left.UpdatedAt == right.UpdatedAt
}

func equalActor(left, right Actor) bool {
	return left.Kind == right.Kind && equalInt64Pointer(left.ID, right.ID)
}

func equalInt64Pointer(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalProjectionSource(left, right *ProjectionSource) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return projectionSourcesEqual(*left, *right)
}
