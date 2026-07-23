package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--version"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("run(--version) exit code = %d, want 0", exitCode)
	}
	if !strings.HasPrefix(stdout.String(), "scrapefs ") {
		t.Errorf("version output = %q, want scrapefs prefix", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("version stderr = %q, want empty", stderr.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("run(--help) exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout.String(), "Usage: scrapefs") {
		t.Errorf("help output = %q, want scrapefs usage", stdout.String())
	}
	if !strings.Contains(stdout.String(), "-h, --help") {
		t.Errorf("help output = %q, want Kong help flag", stdout.String())
	}
	if !strings.Contains(stdout.String(), "--version") {
		t.Errorf("help output = %q, want version flag", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("help stderr = %q, want empty", stderr.String())
	}
}

func TestRunWithoutArgumentsShowsHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(nil, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("run() exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout.String(), "Usage: scrapefs") {
		t.Errorf("output = %q, want scrapefs usage", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"--unknown"}, {"unexpected"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := run(args, &stdout, &stderr)

			if exitCode != 2 {
				t.Fatalf("run(%v) exit code = %d, want 2", args, exitCode)
			}
			if stderr.Len() == 0 {
				t.Errorf("run(%v) stderr is empty", args)
			}
		})
	}
}

func TestRunProjectionBatchValidateReadsStdinAndWritesJSON(t *testing.T) {
	input := []byte(`{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"/srv/Dune.mkv","target":"Movies/Dune.mkv"}]}`)
	service := &fakeBatchService{
		validation: projectionbatch.ValidationResult{
			Schema:           projectionbatch.ValidationSchema,
			Digest:           "sha256:" + strings.Repeat("a", 64),
			NamespaceVersion: 7,
			Valid:            true,
			Entries:          []projectionbatch.ValidationEntry{},
			Warnings:         []string{},
		},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithBatchService(
		[]string{"projection", "batch", "validate", "--file", "-", "--json"},
		bytes.NewReader(input),
		&stdout,
		&stderr,
		service,
		projectionbatch.Caller{UID: 1000, GID: 1001},
	)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if string(service.validated) != string(input) {
		t.Errorf("validated input = %q, want %q", service.validated, input)
	}
	var result projectionbatch.ValidationResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal stdout: %v", err)
	}
	if !reflect.DeepEqual(result, service.validation) {
		t.Errorf("stdout result = %#v, want %#v", result, service.validation)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunProjectionBatchApplyPassesDigestAndCaller(t *testing.T) {
	input := []byte(`{"schema":"scrapefs.projection-batch/v1"}`)
	service := &fakeBatchService{
		applied: projectionbatch.ApplyResult{
			Schema:           projectionbatch.ResultSchema,
			BatchID:          "batch-42",
			Digest:           "sha256:" + strings.Repeat("b", 64),
			NamespaceVersion: 8,
		},
	}
	caller := projectionbatch.Caller{UID: 1000, GID: 1001}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithBatchService(
		[]string{
			"projection", "batch", "apply",
			"--file", "-",
			"--expected-digest", service.applied.Digest,
		},
		bytes.NewReader(input),
		&stdout,
		&stderr,
		service,
		caller,
	)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if string(service.applyRequest.Data) != string(input) ||
		service.applyRequest.ExpectedDigest != service.applied.Digest ||
		service.applyRequest.Caller != caller ||
		service.applyRequest.Actor.Kind != namespace.ActorControl ||
		service.applyRequest.Actor.ID == nil ||
		*service.applyRequest.Actor.ID != int64(caller.UID) {
		t.Errorf("apply request = %#v", service.applyRequest)
	}
}

func TestRunProjectionBatchCommandsWriteMachineReadableErrors(t *testing.T) {
	service := &fakeBatchService{err: projectionbatch.ErrAlreadyUndone}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithBatchService(
		[]string{"projection", "batch", "undo", "batch-42"},
		strings.NewReader(""),
		&stdout,
		&stderr,
		service,
		projectionbatch.Caller{UID: 1000, GID: 1001},
	)

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	var response errorResponse
	if err := json.Unmarshal(stderr.Bytes(), &response); err != nil {
		t.Fatalf("Unmarshal stderr: %v; stderr=%q", err, stderr.String())
	}
	if response.Schema != projectionbatch.ErrorSchema || response.Code != "AlreadyUndone" {
		t.Errorf("error response = %#v", response)
	}
}

func TestRunHelpListsProjectionBatchCommands(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"projection", "batch", "--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	for _, command := range []string{"validate", "apply", "list", "show", "undo"} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("help output %q does not contain %q", stdout.String(), command)
		}
	}
}

type fakeBatchService struct {
	validation   projectionbatch.ValidationResult
	applied      projectionbatch.ApplyResult
	listed       projectionbatch.ListResult
	shown        projectionbatch.DetailResult
	undone       projectionbatch.UndoResult
	err          error
	validated    []byte
	applyRequest projectionbatch.ApplyRequest
	undoRequest  projectionbatch.UndoRequest
}

func (service *fakeBatchService) Validate(
	_ context.Context,
	data []byte,
) (projectionbatch.ValidationResult, error) {
	service.validated = append([]byte(nil), data...)
	return service.validation, service.err
}

func (service *fakeBatchService) Apply(
	_ context.Context,
	request projectionbatch.ApplyRequest,
) (projectionbatch.ApplyResult, error) {
	service.applyRequest = request
	return service.applied, service.err
}

func (service *fakeBatchService) List(
	context.Context,
) (projectionbatch.ListResult, error) {
	return service.listed, service.err
}

func (service *fakeBatchService) Show(
	_ context.Context,
	_ string,
) (projectionbatch.DetailResult, error) {
	return service.shown, service.err
}

func (service *fakeBatchService) Undo(
	_ context.Context,
	request projectionbatch.UndoRequest,
) (projectionbatch.UndoResult, error) {
	service.undoRequest = request
	return service.undone, service.err
}

var _ batchService = (*fakeBatchService)(nil)
