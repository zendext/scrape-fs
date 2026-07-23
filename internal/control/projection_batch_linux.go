package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"

	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projection"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
	"golang.org/x/sys/unix"
)

const (
	DefaultSocketPath             = "/run/scrapefs/scrapefs.sock"
	requestSchema                 = "scrapefs.control-request/v1"
	responseSchema                = "scrapefs.control-response/v1"
	defaultMaxConnections         = 32
	defaultMaxRequestBytes  int64 = 16 << 20
	defaultReadTimeout            = 5 * time.Second
	defaultWriteTimeout           = 30 * time.Second
	defaultAcceptRetryDelay       = 100 * time.Millisecond
)

type BatchService interface {
	Validate(context.Context, []byte) (projectionbatch.ValidationResult, error)
	Apply(context.Context, projectionbatch.ApplyRequest) (projectionbatch.ApplyResult, error)
	List(context.Context) (projectionbatch.ListResult, error)
	Show(context.Context, string) (projectionbatch.DetailResult, error)
	Undo(context.Context, projectionbatch.UndoRequest) (projectionbatch.UndoResult, error)
}

type Client struct {
	socketPath string
}

type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (err *RemoteError) Error() string {
	return err.Message
}

func (err *RemoteError) ErrorCode() string {
	return err.Code
}

type request struct {
	Schema         string `json:"schema"`
	Command        string `json:"command"`
	Batch          []byte `json:"batch,omitempty"`
	ExpectedDigest string `json:"expected_digest,omitempty"`
	BatchID        string `json:"batch_id,omitempty"`
}

type response struct {
	Schema string          `json:"schema"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RemoteError    `json:"error,omitempty"`
}

type serverLimits struct {
	maxConnections   int
	maxRequestBytes  int64
	readTimeout      time.Duration
	writeTimeout     time.Duration
	acceptRetryDelay time.Duration
}

func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

func (client *Client) Validate(
	ctx context.Context,
	data []byte,
) (projectionbatch.ValidationResult, error) {
	var result projectionbatch.ValidationResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "projection.batch.validate",
		Batch:   data,
	}, &result)
	return result, err
}

func (client *Client) Apply(
	ctx context.Context,
	apply projectionbatch.ApplyRequest,
) (projectionbatch.ApplyResult, error) {
	var result projectionbatch.ApplyResult
	err := client.call(ctx, request{
		Schema:         requestSchema,
		Command:        "projection.batch.apply",
		Batch:          apply.Data,
		ExpectedDigest: apply.ExpectedDigest,
	}, &result)
	return result, err
}

func (client *Client) List(
	ctx context.Context,
) (projectionbatch.ListResult, error) {
	var result projectionbatch.ListResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "projection.batch.list",
	}, &result)
	return result, err
}

func (client *Client) Show(
	ctx context.Context,
	batchID string,
) (projectionbatch.DetailResult, error) {
	var result projectionbatch.DetailResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "projection.batch.show",
		BatchID: batchID,
	}, &result)
	return result, err
}

func (client *Client) Undo(
	ctx context.Context,
	undo projectionbatch.UndoRequest,
) (projectionbatch.UndoResult, error) {
	var result projectionbatch.UndoResult
	err := client.call(ctx, request{
		Schema:  requestSchema,
		Command: "projection.batch.undo",
		BatchID: undo.BatchID,
	}, &result)
	return result, err
}

func (client *Client) call(
	ctx context.Context,
	request request,
	result any,
) error {
	connection, err := (&net.Dialer{}).DialContext(
		ctx,
		"unix",
		client.socketPath,
	)
	if err != nil {
		return fmt.Errorf("connect ScrapeFS control socket %q: %w", client.socketPath, err)
	}
	defer connection.Close()
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			connection.Close()
		case <-finished:
		}
	}()
	defer close(finished)
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set control request deadline: %w", err)
		}
	}

	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return fmt.Errorf("write control request: %w", err)
	}
	if unixConnection, ok := connection.(*net.UnixConn); ok {
		if err := unixConnection.CloseWrite(); err != nil {
			return fmt.Errorf("finish control request: %w", err)
		}
	}
	var response response
	decoder := json.NewDecoder(connection)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("read control response: %w", err)
	}
	if response.Schema != responseSchema {
		return fmt.Errorf("invalid control response schema %q", response.Schema)
	}
	if response.Error != nil {
		return response.Error
	}
	if len(response.Result) == 0 {
		return fmt.Errorf("control response has no result")
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode control result: %w", err)
	}
	return nil
}

func Serve(
	ctx context.Context,
	socketPath string,
	mode fs.FileMode,
	service BatchService,
) error {
	return serveWithLimits(ctx, socketPath, mode, service, serverLimits{
		maxConnections:   defaultMaxConnections,
		maxRequestBytes:  defaultMaxRequestBytes,
		readTimeout:      defaultReadTimeout,
		writeTimeout:     defaultWriteTimeout,
		acceptRetryDelay: defaultAcceptRetryDelay,
	})
}

func serveWithLimits(
	ctx context.Context,
	socketPath string,
	mode fs.FileMode,
	service BatchService,
	limits serverLimits,
) error {
	if service == nil {
		return fmt.Errorf("Projection Batch control service must not be nil")
	}
	if err := validateServerLimits(limits); err != nil {
		return err
	}
	if err := prepareSocketPath(socketPath); err != nil {
		return err
	}

	address := &net.UnixAddr{Name: socketPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return fmt.Errorf("listen on ScrapeFS control socket %q: %w", socketPath, err)
	}
	listener.SetUnlinkOnClose(true)
	defer listener.Close()
	if err := os.Chmod(socketPath, mode.Perm()); err != nil {
		return fmt.Errorf("set ScrapeFS control socket mode: %w", err)
	}

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-stopped:
		}
	}()
	defer close(stopped)

	var handlers sync.WaitGroup
	defer handlers.Wait()
	slots := make(chan struct{}, limits.maxConnections)
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		connection, err := listener.AcceptUnix()
		if err != nil {
			<-slots
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if retryableAcceptError(err) {
				timer := time.NewTimer(limits.acceptRetryDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
					continue
				}
			}
			return fmt.Errorf("accept ScrapeFS control connection: %w", err)
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() {
				<-slots
			}()
			handleConnection(ctx, connection, service, limits)
		}()
	}
}

func validateServerLimits(limits serverLimits) error {
	if limits.maxConnections <= 0 ||
		limits.maxRequestBytes <= 0 ||
		limits.readTimeout <= 0 ||
		limits.writeTimeout <= 0 ||
		limits.acceptRetryDelay <= 0 {
		return fmt.Errorf("invalid control server resource limits")
	}
	return nil
}

func retryableAcceptError(err error) bool {
	if errors.Is(err, unix.EMFILE) ||
		errors.Is(err, unix.ENFILE) ||
		errors.Is(err, unix.ENOBUFS) ||
		errors.Is(err, unix.ENOMEM) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Temporary()
}

func prepareSocketPath(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect ScrapeFS control socket %q: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf(
			"ScrapeFS control socket path %q exists and is not a socket",
			socketPath,
		)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove stale ScrapeFS control socket %q: %w", socketPath, err)
	}
	return nil
}

func handleConnection(
	ctx context.Context,
	connection *net.UnixConn,
	service BatchService,
	limits serverLimits,
) {
	defer connection.Close()
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			connection.Close()
		case <-finished:
		}
	}()
	defer close(finished)
	respond := func(result any, err error) {
		if deadlineErr := connection.SetWriteDeadline(
			time.Now().Add(limits.writeTimeout),
		); deadlineErr != nil {
			return
		}
		writeResponse(connection, result, err)
	}

	credentials, err := peerCredentials(connection)
	if err != nil {
		respond(nil, err)
		return
	}

	var request request
	if err := connection.SetReadDeadline(time.Now().Add(limits.readTimeout)); err != nil {
		respond(nil, fmt.Errorf("set control request deadline: %w", err))
		return
	}
	limited := &io.LimitedReader{
		R: connection,
		N: limits.maxRequestBytes + 1,
	}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if limited.N == 0 {
			err = fmt.Errorf(
				"control request exceeds %d bytes",
				limits.maxRequestBytes,
			)
		} else {
			err = fmt.Errorf("decode control request: %w", err)
		}
		respond(nil, err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("control request contains more than one JSON value")
		}
		respond(nil, err)
		return
	}
	if limited.N == 0 {
		respond(nil, fmt.Errorf(
			"control request exceeds %d bytes",
			limits.maxRequestBytes,
		))
		return
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		respond(nil, fmt.Errorf("clear control request deadline: %w", err))
		return
	}
	if request.Schema != requestSchema {
		respond(
			nil,
			fmt.Errorf("invalid control request schema %q", request.Schema),
		)
		return
	}

	actorID := int64(credentials.Uid)
	actor := namespace.Actor{Kind: namespace.ActorControl, ID: &actorID}
	caller := projectionbatch.Caller{
		UID: credentials.Uid,
		GID: credentials.Gid,
	}
	var result any
	switch request.Command {
	case "projection.batch.validate":
		result, err = service.Validate(ctx, request.Batch)
	case "projection.batch.apply":
		result, err = service.Apply(ctx, projectionbatch.ApplyRequest{
			Data:           request.Batch,
			ExpectedDigest: request.ExpectedDigest,
			Actor:          actor,
			Caller:         caller,
		})
	case "projection.batch.list":
		result, err = service.List(ctx)
	case "projection.batch.show":
		result, err = service.Show(ctx, request.BatchID)
	case "projection.batch.undo":
		result, err = service.Undo(ctx, projectionbatch.UndoRequest{
			BatchID: request.BatchID,
			Actor:   actor,
			Caller:  caller,
		})
	default:
		err = fmt.Errorf("unsupported control command %q", request.Command)
	}
	respond(result, err)
}

func peerCredentials(connection *net.UnixConn) (*unix.Ucred, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("access control connection descriptor: %w", err)
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(
			int(fd),
			unix.SOL_SOCKET,
			unix.SO_PEERCRED,
		)
	}); err != nil {
		return nil, fmt.Errorf("inspect control peer credentials: %w", err)
	}
	if credentialErr != nil {
		return nil, fmt.Errorf("read control peer credentials: %w", credentialErr)
	}
	return credentials, nil
}

func writeResponse(connection io.Writer, result any, err error) {
	response := response{Schema: responseSchema}
	if err != nil {
		response.Error = &RemoteError{
			Code:    classifyError(err),
			Message: err.Error(),
		}
	} else {
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			response.Error = &RemoteError{
				Code:    "Internal",
				Message: fmt.Sprintf("encode control result: %v", encodeErr),
			}
		} else {
			response.Result = encoded
		}
	}
	_ = json.NewEncoder(connection).Encode(response)
}

func classifyError(err error) string {
	switch {
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

var _ BatchService = (*Client)(nil)
