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

	"github.com/zendext/scrape-fs/internal/library"
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
	Schema             string `json:"schema"`
	Command            string `json:"command"`
	Batch              []byte `json:"batch,omitempty"`
	ExpectedDigest     string `json:"expected_digest,omitempty"`
	BatchID            string `json:"batch_id,omitempty"`
	LibraryName        string `json:"library_name,omitempty"`
	NewLibraryName     string `json:"new_library_name,omitempty"`
	ProfileName        string `json:"profile_name,omitempty"`
	ProfileVersion     int64  `json:"profile_version,omitempty"`
	SourcePath         string `json:"source_path,omitempty"`
	Target             string `json:"target,omitempty"`
	NewTarget          string `json:"new_target,omitempty"`
	ConfirmationDigest string `json:"confirmation_digest,omitempty"`
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

type serverOptions struct {
	limits            serverLimits
	security          serverSecurity
	readPeerIdentity  peerIdentityReader
	ready             chan<- struct{}
	gcService         GCService
	libraryService    LibraryService
	projectionService ProjectionService
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
	administratorUID uint32,
	service BatchService,
	libraryService LibraryService,
	projectionService ProjectionService,
	gcService GCService,
) error {
	if libraryService == nil {
		return fmt.Errorf("Library control service must not be nil")
	}
	if projectionService == nil {
		return fmt.Errorf("Projection control service must not be nil")
	}
	if gcService == nil {
		return fmt.Errorf("GC control service must not be nil")
	}
	controlGroupID, err := resolveGroupID(ControlGroupName, systemGroupLookup)
	if err != nil {
		return err
	}
	return serveWithOptions(ctx, socketPath, mode, service, serverOptions{
		limits: defaultServerLimits(),
		security: serverSecurity{
			daemonUID:        uint32(os.Geteuid()),
			administratorUID: administratorUID,
			controlGroupID:   controlGroupID,
		},
		readPeerIdentity:  readPeerIdentity,
		gcService:         gcService,
		libraryService:    libraryService,
		projectionService: projectionService,
	})
}

func defaultServerLimits() serverLimits {
	return serverLimits{
		maxConnections:   defaultMaxConnections,
		maxRequestBytes:  defaultMaxRequestBytes,
		readTimeout:      defaultReadTimeout,
		writeTimeout:     defaultWriteTimeout,
		acceptRetryDelay: defaultAcceptRetryDelay,
	}
}

func serveWithLimits(
	ctx context.Context,
	socketPath string,
	mode fs.FileMode,
	service BatchService,
	limits serverLimits,
) error {
	return serveWithOptions(ctx, socketPath, mode, service, serverOptions{
		limits: limits,
		security: serverSecurity{
			daemonUID:        uint32(os.Geteuid()),
			administratorUID: uint32(os.Geteuid()),
			controlGroupID:   uint32(os.Getegid()),
		},
		readPeerIdentity: readPeerIdentity,
	})
}

func serveWithOptions(
	ctx context.Context,
	socketPath string,
	mode fs.FileMode,
	service BatchService,
	options serverOptions,
) error {
	if service == nil {
		return fmt.Errorf("Projection Batch control service must not be nil")
	}
	if err := validateServerLimits(options.limits); err != nil {
		return err
	}
	if options.readPeerIdentity == nil {
		return fmt.Errorf("control peer identity reader must not be nil")
	}
	prepared, err := prepareSocketPath(socketPath, options.security)
	if err != nil {
		return err
	}
	defer prepared.Close()

	listener, err := prepared.listen()
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := prepared.setMetadata(mode); err != nil {
		return err
	}
	if err := prepared.grantRoleAccess(options.security); err != nil {
		return err
	}
	if err := prepared.verifyConfiguredSocket(); err != nil {
		return err
	}
	if options.ready != nil {
		close(options.ready)
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
	slots := make(chan struct{}, options.limits.maxConnections)
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
				timer := time.NewTimer(options.limits.acceptRetryDelay)
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
			handleConnection(ctx, connection, service, options)
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

func handleConnection(
	ctx context.Context,
	connection *net.UnixConn,
	service BatchService,
	options serverOptions,
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
			time.Now().Add(options.limits.writeTimeout),
		); deadlineErr != nil {
			return
		}
		writeResponse(connection, result, err)
	}

	identity, err := options.readPeerIdentity(connection)
	if err != nil {
		respond(nil, err)
		return
	}

	var request request
	if err := connection.SetReadDeadline(time.Now().Add(options.limits.readTimeout)); err != nil {
		respond(nil, fmt.Errorf("set control request deadline: %w", err))
		return
	}
	limited := &io.LimitedReader{
		R: connection,
		N: options.limits.maxRequestBytes + 1,
	}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if limited.N == 0 {
			err = fmt.Errorf(
				"control request exceeds %d bytes",
				options.limits.maxRequestBytes,
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
			options.limits.maxRequestBytes,
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
	if requiredCommandRole(request.Command) == commandRoleUnknown {
		respond(nil, fmt.Errorf("unsupported control command %q", request.Command))
		return
	}
	if err := options.security.authorize(request.Command, identity); err != nil {
		respond(nil, err)
		return
	}

	actorID := int64(identity.UID)
	actor := namespace.Actor{Kind: namespace.ActorControl, ID: &actorID}
	caller := projectionbatch.Caller{
		UID: identity.UID,
		GID: identity.GID,
	}
	var result any
	switch request.Command {
	case "library.create":
		if options.libraryService == nil {
			err = fmt.Errorf("Library control service is unavailable")
			break
		}
		var created namespace.Library
		created, err = options.libraryService.Create(ctx, library.CreateRequest{
			Name: request.LibraryName,
			Profile: library.ProfileIdentifier{
				Name:    request.ProfileName,
				Version: request.ProfileVersion,
			},
			Actor: actor,
			UID:   identity.UID,
			GID:   identity.GID,
			Modes: library.DefaultModes,
		})
		if err == nil {
			result = libraryResult(created)
		}
	case "library.list":
		if options.libraryService == nil {
			err = fmt.Errorf("Library control service is unavailable")
			break
		}
		var libraries []namespace.Library
		libraries, err = options.libraryService.List(ctx)
		if err == nil {
			result = libraryListResult(libraries)
		}
	case "library.rename":
		if options.libraryService == nil {
			err = fmt.Errorf("Library control service is unavailable")
			break
		}
		var renamed namespace.Library
		renamed, err = options.libraryService.Rename(ctx, library.RenameRequest{
			Name:    request.LibraryName,
			NewName: request.NewLibraryName,
			Actor:   actor,
		})
		if err == nil {
			result = libraryResult(renamed)
		}
	case "library.set-profile":
		if options.libraryService == nil {
			err = fmt.Errorf("Library control service is unavailable")
			break
		}
		var updated namespace.Library
		updated, err = options.libraryService.SetProfile(ctx, library.SetProfileRequest{
			Name: request.LibraryName,
			Profile: library.ProfileIdentifier{
				Name:    request.ProfileName,
				Version: request.ProfileVersion,
			},
			Actor: actor,
		})
		if err == nil {
			result = libraryResult(updated)
		}
	case "projection.create":
		if options.projectionService == nil {
			err = fmt.Errorf("Projection control service is unavailable")
			break
		}
		var created namespace.Node
		created, err = options.projectionService.Create(ctx, projection.CreateRequest{
			SourcePath: request.SourcePath,
			Target:     request.Target,
			Actor:      actor,
		})
		if err == nil {
			result = projectionResult(created, request.Target)
		}
	case "projection.list":
		if options.projectionService == nil {
			err = fmt.Errorf("Projection control service is unavailable")
			break
		}
		var projections []namespace.Projection
		projections, err = options.projectionService.List(ctx, projection.ListRequest{
			LibraryName: request.LibraryName,
		})
		if err == nil {
			result = projectionListResult(projections)
		}
	case "projection.move":
		if options.projectionService == nil {
			err = fmt.Errorf("Projection control service is unavailable")
			break
		}
		var moved namespace.Projection
		moved, err = options.projectionService.Move(ctx, projection.MoveRequest{
			Target:    request.Target,
			NewTarget: request.NewTarget,
			Actor:     actor,
		})
		if err == nil {
			result = projectionRecordResult(moved)
		}
	case "projection.remove":
		if options.projectionService == nil {
			err = fmt.Errorf("Projection control service is unavailable")
			break
		}
		var removed namespace.Projection
		removed, err = options.projectionService.Remove(ctx, projection.RemoveRequest{
			Target: request.Target,
			Actor:  actor,
		})
		if err == nil {
			result = projectionRecordResult(removed)
		}
	case "projection.rebind":
		if options.projectionService == nil {
			err = fmt.Errorf("Projection control service is unavailable")
			break
		}
		var rebound projection.RebindResult
		rebound, err = options.projectionService.Rebind(ctx, projection.RebindRequest{
			Target:             request.Target,
			SourcePath:         request.SourcePath,
			ConfirmationDigest: request.ConfirmationDigest,
			Actor:              actor,
		})
		if err == nil {
			result = projectionRebindResult(rebound)
		}
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
	case "gc":
		if options.gcService == nil {
			err = fmt.Errorf("GC control service is unavailable")
			break
		}
		result, err = options.gcService.GarbageCollect(ctx)
	default:
		err = fmt.Errorf("unsupported control command %q", request.Command)
	}
	respond(result, err)
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
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	switch {
	case errors.Is(err, ErrPermissionDenied):
		return "PermissionDenied"
	case errors.Is(err, projectionbatch.ErrInvalidBatch):
		return "InvalidBatch"
	case errors.Is(err, projectionbatch.ErrDigestMismatch):
		return "DigestMismatch"
	case errors.Is(err, namespace.ErrLibraryNotFound):
		return "LibraryNotFound"
	case errors.Is(err, namespace.ErrProfileNotFound):
		return "ProfileNotFound"
	case errors.Is(err, library.ErrAlreadyExists):
		return "LibraryExists"
	case errors.Is(err, namespace.ErrInvalidLibrary):
		return "InvalidLibrary"
	case errors.Is(err, namespace.ErrInvalidPath):
		return "InvalidPath"
	case errors.Is(err, projection.ErrUnsafeSource):
		return "UnsafeSource"
	case errors.Is(err, projection.ErrSourceNotFound):
		return "SourceNotFound"
	case errors.Is(err, projection.ErrSourcePermissionDenied):
		return "SourcePermissionDenied"
	case errors.Is(err, projection.ErrSourceNotRegular):
		return "SourceNotRegular"
	case errors.Is(err, namespace.ErrConflict):
		return "TargetConflict"
	case errors.Is(err, namespace.ErrCrossLibrary):
		return "CrossLibrary"
	case errors.Is(err, projection.ErrProjectionNotFound):
		return "ProjectionNotFound"
	case errors.Is(err, projection.ErrConfirmationMismatch):
		return "ConfirmationMismatch"
	case errors.Is(err, projectionbatch.ErrBatchNotFound):
		return "BatchNotFound"
	case errors.Is(err, projectionbatch.ErrAlreadyUndone):
		return "AlreadyUndone"
	default:
		return "Internal"
	}
}

var _ BatchService = (*Client)(nil)
