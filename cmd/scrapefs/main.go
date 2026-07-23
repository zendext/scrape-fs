package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/zendext/scrape-fs/internal/buildinfo"
	"github.com/zendext/scrape-fs/internal/control"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

type cli struct {
	Projection projectionCLI    `cmd:"" help:"Manage Projections."`
	Socket     string           `help:"Path to the ScrapeFS control socket." default:"${default_socket}"`
	Version    kong.VersionFlag `help:"Print version and exit."`
}

type projectionCLI struct {
	Batch projectionBatchCLI `cmd:"" help:"Validate, apply, inspect, and undo Projection Batches."`
}

type projectionBatchCLI struct {
	Validate projectionBatchValidateCLI `cmd:"" help:"Validate a Projection Batch without persistence."`
	Apply    projectionBatchApplyCLI    `cmd:"" help:"Atomically apply a Projection Batch."`
	List     projectionBatchListCLI     `cmd:"" help:"List applied and undone Projection Batches."`
	Show     projectionBatchShowCLI     `cmd:"" help:"Show Projection Batch provenance and entries."`
	Undo     projectionBatchUndoCLI     `cmd:"" help:"Undo a Projection Batch atomically."`
}

type projectionBatchValidateCLI struct {
	File string `help:"Read the Projection Batch JSON from this path, or - for stdin." required:""`
	JSON bool   `help:"Write machine-readable JSON."`
}

type projectionBatchApplyCLI struct {
	File           string `help:"Read the Projection Batch JSON from this path, or - for stdin." required:""`
	ExpectedDigest string `help:"Require this canonical Projection Batch digest." required:""`
}

type projectionBatchListCLI struct{}

type projectionBatchShowCLI struct {
	BatchID string `arg:"" name:"batch-id" help:"Stable Projection Batch ID."`
}

type projectionBatchUndoCLI struct {
	BatchID string `arg:"" name:"batch-id" help:"Stable Projection Batch ID."`
}

type batchService interface {
	Validate(context.Context, []byte) (projectionbatch.ValidationResult, error)
	Apply(context.Context, projectionbatch.ApplyRequest) (projectionbatch.ApplyResult, error)
	List(context.Context) (projectionbatch.ListResult, error)
	Show(context.Context, string) (projectionbatch.DetailResult, error)
	Undo(context.Context, projectionbatch.UndoRequest) (projectionbatch.UndoResult, error)
}

type errorResponse struct {
	Schema  string `json:"schema"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithBatchService(
		args,
		os.Stdin,
		stdout,
		stderr,
		nil,
		projectionbatch.Caller{
			UID: uint32(os.Geteuid()),
			GID: uint32(os.Getegid()),
		},
	)
}

func runWithBatchService(
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	service batchService,
	caller projectionbatch.Caller,
) int {
	commandLine := cli{}
	requestedExitCode := -1
	parser, err := kong.New(
		&commandLine,
		kong.Name("scrapefs"),
		kong.Description("ScrapeFS control CLI."),
		kong.Vars{
			"default_socket": control.DefaultSocketPath,
			"version":        "scrapefs " + buildinfo.Version,
		},
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) {
			requestedExitCode = code
		}),
	)
	if err != nil {
		fmt.Fprintf(stderr, "scrapefs: %v\n", err)
		return 1
	}

	if len(args) == 0 {
		args = []string{"--help"}
	}
	parsed, err := parser.Parse(args)
	if requestedExitCode >= 0 {
		return requestedExitCode
	}
	if err != nil {
		fmt.Fprintf(stderr, "scrapefs: %v\n", err)
		return 2
	}

	command := parsed.Command()
	if command == "" {
		if err := parsed.PrintUsage(false); err != nil {
			fmt.Fprintf(stderr, "scrapefs: %v\n", err)
			return 1
		}
		return 0
	}

	ctx := context.Background()
	if service == nil {
		service = control.NewClient(commandLine.Socket)
	}
	actorID := int64(caller.UID)
	actor := namespace.Actor{Kind: namespace.ActorControl, ID: &actorID}
	var result any
	switch {
	case strings.HasPrefix(command, "projection batch validate"):
		data, err := readBatch(commandLine.Projection.Batch.Validate.File, stdin)
		if err != nil {
			return writeCommandError(stderr, err)
		}
		result, err = service.Validate(ctx, data)
		if err != nil {
			return writeCommandError(stderr, err)
		}
	case strings.HasPrefix(command, "projection batch apply"):
		data, err := readBatch(commandLine.Projection.Batch.Apply.File, stdin)
		if err != nil {
			return writeCommandError(stderr, err)
		}
		result, err = service.Apply(ctx, projectionbatch.ApplyRequest{
			Data:           data,
			ExpectedDigest: commandLine.Projection.Batch.Apply.ExpectedDigest,
			Actor:          actor,
			Caller:         caller,
		})
		if err != nil {
			return writeCommandError(stderr, err)
		}
	case strings.HasPrefix(command, "projection batch list"):
		result, err = service.List(ctx)
		if err != nil {
			return writeCommandError(stderr, err)
		}
	case strings.HasPrefix(command, "projection batch show"):
		result, err = service.Show(ctx, commandLine.Projection.Batch.Show.BatchID)
		if err != nil {
			return writeCommandError(stderr, err)
		}
	case strings.HasPrefix(command, "projection batch undo"):
		result, err = service.Undo(ctx, projectionbatch.UndoRequest{
			BatchID: commandLine.Projection.Batch.Undo.BatchID,
			Actor:   actor,
			Caller:  caller,
		})
		if err != nil {
			return writeCommandError(stderr, err)
		}
	default:
		if err := parsed.PrintUsage(false); err != nil {
			fmt.Fprintf(stderr, "scrapefs: %v\n", err)
			return 1
		}
		return 0
	}

	if err := writeJSON(stdout, result); err != nil {
		fmt.Fprintf(stderr, "scrapefs: write JSON output: %v\n", err)
		return 1
	}
	return 0
}

func readBatch(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read Projection Batch from stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Projection Batch %q: %w", path, err)
	}
	return data, nil
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeCommandError(stderr io.Writer, err error) int {
	response := errorResponse{
		Schema:  projectionbatch.ErrorSchema,
		Code:    errorCode(err),
		Message: err.Error(),
	}
	if encodeErr := writeJSON(stderr, response); encodeErr != nil {
		fmt.Fprintf(stderr, "scrapefs: write JSON error: %v\n", encodeErr)
	}
	return 1
}

func errorCode(err error) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	switch {
	case errors.Is(err, control.ErrPermissionDenied):
		return "PermissionDenied"
	case errors.Is(err, projectionbatch.ErrInvalidBatch):
		return "InvalidBatch"
	case errors.Is(err, projectionbatch.ErrDigestMismatch):
		return "DigestMismatch"
	case errors.Is(err, namespace.ErrLibraryNotFound):
		return "LibraryNotFound"
	case errors.Is(err, namespace.ErrProfileNotFound):
		return "ProfileNotFound"
	case errors.Is(err, namespace.ErrInvalidPath):
		return "InvalidPath"
	case errors.Is(err, projection.ErrUnsafeSource):
		return "UnsafeSource"
	case errors.Is(err, projection.ErrSourceNotRegular):
		return "SourceNotRegular"
	case errors.Is(err, namespace.ErrConflict):
		return "TargetConflict"
	case errors.Is(err, projectionbatch.ErrBatchNotFound):
		return "BatchNotFound"
	case errors.Is(err, projectionbatch.ErrAlreadyUndone):
		return "AlreadyUndone"
	default:
		return "Internal"
	}
}
