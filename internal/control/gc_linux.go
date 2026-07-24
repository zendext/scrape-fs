package control

import (
	"context"

	"github.com/zendext/scrape-fs/internal/objectstore"
)

type GCService interface {
	GarbageCollect(context.Context) (objectstore.GCResult, error)
}

func (client *Client) GarbageCollect(
	ctx context.Context,
) (objectstore.GCResult, error) {
	var result objectstore.GCResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "gc",
	}, &result)
	return result, err
}
