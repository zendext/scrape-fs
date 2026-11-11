package control

import (
	"context"

	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
)

type ManagementService interface {
	Browse(context.Context, string, string) (library.BrowseResult, error)
	CreateDirectory(context.Context, string, string, string, int64, namespace.Actor) error
	MoveDirectory(context.Context, string, string, string, int64, namespace.Actor) error
	PreviewRemoval(context.Context, string, string) (library.RemovalPreview, error)
	RemoveTree(context.Context, string, string, string, namespace.Actor) error
}

func (client *Client) Browse(ctx context.Context, name, path string) (library.BrowseResult, error) {
	var result library.BrowseResult
	err := client.call(ctx, request{Schema: requestSchema, Command: "directory.list", LibraryName: name, Path: path}, &result)
	return result, err
}

func (client *Client) CreateDirectory(ctx context.Context, name, path, child string, id int64) error {
	var result struct{}
	return client.call(ctx, request{Schema: requestSchema, Command: "directory.create", LibraryName: name, Path: path, NewTarget: child, NodeID: id}, &result)
}

func (client *Client) MoveDirectory(ctx context.Context, name, path, target string, id int64) error {
	var result struct{}
	return client.call(ctx, request{Schema: requestSchema, Command: "directory.move", LibraryName: name, Path: path, NewTarget: target, NodeID: id}, &result)
}

func (client *Client) PreviewRemoval(ctx context.Context, name, path string) (library.RemovalPreview, error) {
	var result library.RemovalPreview
	err := client.call(ctx, request{Schema: requestSchema, Command: "tree.preview", LibraryName: name, Path: path}, &result)
	return result, err
}

func (client *Client) RemoveTree(ctx context.Context, name, path, digest string) error {
	var result struct{}
	return client.call(ctx, request{Schema: requestSchema, Command: "tree.remove", LibraryName: name, Path: path, ExpectedDigest: digest}, &result)
}
