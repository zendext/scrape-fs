package control

import (
	"context"

	"github.com/zendext/scrape-fs/internal/diagnostics"
)

type DiagnosticsService interface {
	Status(context.Context) diagnostics.StatusResult
	Doctor(context.Context) diagnostics.DoctorResult
}

func (client *Client) Status(ctx context.Context) (diagnostics.StatusResult, error) {
	var result diagnostics.StatusResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "status",
	}, &result)
	return result, err
}

func (client *Client) Doctor(ctx context.Context) (diagnostics.DoctorResult, error) {
	var result diagnostics.DoctorResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "doctor",
	}, &result)
	return result, err
}
