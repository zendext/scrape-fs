package namespace

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRepositoryReadsNodesAndDirectChildren(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()

	root, err := repository.GlobalRoot(ctx)
	if err != nil {
		t.Fatalf("GlobalRoot: %v", err)
	}

	var libraryRoot Node
	var directory Node
	var projection Node
	if _, err := repository.Update(
		ctx,
		testOperation("namespace.read-test"),
		func(transaction *Transaction) error {
			var err error
			libraryRoot, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: root.ID,
				Name:     "library",
				Kind:     LibraryRoot,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o775,
					UID:  1000,
					GID:  1001,
				},
			})
			if err != nil {
				return err
			}
			directory, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: libraryRoot.ID,
				Name:     "Movies",
				Kind:     Directory,
				Creator:  Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o750,
					UID:  1000,
					GID:  1001,
				},
			})
			if err != nil {
				return err
			}
			projection, err = transaction.CreateNode(ctx, CreateNode{
				ParentID: libraryRoot.ID,
				Name:     "README.txt",
				Kind:     ProjectionFile,
				Source: &ProjectionSource{
					Path:          "/srv/media/README.txt",
					DeviceID:      1,
					Inode:         2,
					Size:          3,
					AtimeNS:       4,
					MtimeNS:       5,
					SourceCtimeNS: 6,
					VerifiedAtNS:  7,
				},
				Creator: Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: 0o440,
					UID:  1000,
					GID:  1001,
				},
			})
			return err
		},
	); err != nil {
		t.Fatalf("create test namespace: %v", err)
	}

	found, err := repository.Node(ctx, directory.ID)
	if err != nil {
		t.Fatalf("Node: %v", err)
	}
	if !reflect.DeepEqual(found, directory) {
		t.Errorf("Node = %#v, want %#v", found, directory)
	}

	found, err = repository.Child(ctx, libraryRoot.ID, projection.Name)
	if err != nil {
		t.Fatalf("Child: %v", err)
	}
	if !reflect.DeepEqual(found, projection) {
		t.Errorf("Child = %#v, want %#v", found, projection)
	}

	children, err := repository.Children(ctx, libraryRoot.ID)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(children) != 2 ||
		!reflect.DeepEqual(children[0], directory) ||
		!reflect.DeepEqual(children[1], projection) {
		t.Errorf(
			"Children = %#v, want name-ordered [%#v %#v]",
			children,
			directory,
			projection,
		)
	}

	if _, err := repository.Node(ctx, 1<<62); !errors.Is(err, ErrNotFound) {
		t.Errorf("Node missing error = %v, want ErrNotFound", err)
	}
	if _, err := repository.Child(ctx, libraryRoot.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Child missing error = %v, want ErrNotFound", err)
	}
}

func TestRepositoryListsProjectionsDeterministicallyAndByLibrary(t *testing.T) {
	opened := openTestDatabase(t)
	repository := NewRepository(opened)
	ctx := context.Background()

	zeta := createProjectionListLibrary(t, repository, "zeta")
	alpha := createProjectionListLibrary(t, repository, "alpha")
	createProjectionListNode(t, repository, zeta, "Movies/Zeta.mkv", "/srv/zeta.mkv", 30)
	createProjectionListNode(t, repository, alpha, "Movies/Zulu.mkv", "/srv/zulu.mkv", 20)
	createProjectionListNode(t, repository, alpha, "Movies/Alpha.mkv", "/srv/alpha.mkv", 10)

	listed, err := repository.Projections(ctx, "")
	if err != nil {
		t.Fatalf("Projections: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("Projections length = %d, want 3", len(listed))
	}
	gotTargets := []string{
		listed[0].LibraryName + ":/" + listed[0].Target,
		listed[1].LibraryName + ":/" + listed[1].Target,
		listed[2].LibraryName + ":/" + listed[2].Target,
	}
	wantTargets := []string{
		"alpha:/Movies/Alpha.mkv",
		"alpha:/Movies/Zulu.mkv",
		"zeta:/Movies/Zeta.mkv",
	}
	if !reflect.DeepEqual(gotTargets, wantTargets) {
		t.Errorf("Projection targets = %#v, want %#v", gotTargets, wantTargets)
	}
	if listed[0].Node.Source == nil || listed[0].Node.Source.Path != "/srv/alpha.mkv" {
		t.Errorf("first Projection source = %#v", listed[0].Node.Source)
	}

	scoped, err := repository.Projections(ctx, "zeta")
	if err != nil {
		t.Fatalf("Projections(zeta): %v", err)
	}
	if len(scoped) != 1 ||
		scoped[0].LibraryID != zeta.ID ||
		scoped[0].Target != "Movies/Zeta.mkv" {
		t.Errorf("Projections(zeta) = %#v", scoped)
	}

	if _, err := repository.Projections(ctx, "missing"); !errors.Is(err, ErrLibraryNotFound) {
		t.Errorf("Projections(missing) error = %v, want ErrLibraryNotFound", err)
	}
}

func createProjectionListLibrary(
	t *testing.T,
	repository *Repository,
	name string,
) Library {
	t.Helper()

	var created Library
	_, err := repository.Update(
		context.Background(),
		Operation{
			Name:  "library.create",
			Actor: Actor{Kind: ActorSystem},
			Details: OperationDetails{
				Paths:     []string{name},
				NodeKinds: []NodeKind{LibraryRoot},
			},
		},
		func(transaction *Transaction) error {
			var err error
			created, err = transaction.CreateLibrary(context.Background(), CreateLibrary{
				Name:           name,
				ProfileName:    "jellyfin",
				ProfileVersion: 1,
				Creator:        Actor{Kind: ActorSystem},
				UID:            1000,
				GID:            1000,
				RootMode:       0o775,
				DirectoryMode:  0o775,
				ProjectionMode: 0o444,
				OverlayMode:    0o664,
			})
			return err
		},
	)
	if err != nil {
		t.Fatalf("CreateLibrary(%q): %v", name, err)
	}
	return created
}

func createProjectionListNode(
	t *testing.T,
	repository *Repository,
	library Library,
	target string,
	sourcePath string,
	inode int64,
) {
	t.Helper()

	components, err := ValidateRelativePath(target)
	if err != nil {
		t.Fatalf("ValidateRelativePath(%q): %v", target, err)
	}
	_, err = repository.Update(
		context.Background(),
		Operation{
			Name:  "projection.create",
			Actor: Actor{Kind: ActorSystem},
			Details: OperationDetails{
				Paths:     []string{library.Name + ":/" + target},
				NodeKinds: []NodeKind{ProjectionFile},
			},
		},
		func(transaction *Transaction) error {
			parent, err := transaction.EnsureManagedDirectories(
				context.Background(),
				library.RootNodeID,
				components[:len(components)-1],
				Attributes{
					Mode: library.DirectoryMode,
					UID:  library.UID,
					GID:  library.GID,
				},
				Actor{Kind: ActorSystem},
			)
			if err != nil {
				return err
			}
			_, err = transaction.CreateNode(context.Background(), CreateNode{
				ParentID: parent.ID,
				Name:     components[len(components)-1],
				Kind:     ProjectionFile,
				Source: &ProjectionSource{
					Path:          sourcePath,
					DeviceID:      1,
					Inode:         inode,
					Size:          inode,
					AtimeNS:       inode + 1,
					MtimeNS:       inode + 2,
					SourceCtimeNS: inode + 3,
					VerifiedAtNS:  inode + 4,
				},
				Creator: Actor{Kind: ActorSystem},
				Attributes: Attributes{
					Mode: library.ProjectionMode,
					UID:  library.UID,
					GID:  library.GID,
				},
			})
			return err
		},
	)
	if err != nil {
		t.Fatalf("create Projection %q: %v", target, err)
	}
}
