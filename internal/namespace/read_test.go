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
