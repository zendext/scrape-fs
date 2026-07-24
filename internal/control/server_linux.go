package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"sync"
	"time"
)

type Server struct {
	listener   *net.UnixListener
	prepared   *preparedSocketPath
	mode       fs.FileMode
	service    BatchService
	options    serverOptions
	stopping   chan struct{}
	acceptDone chan struct{}
	slots      chan struct{}
	handlers   sync.WaitGroup
	stopOnce   sync.Once
	closeOnce  sync.Once
	mu         sync.Mutex
	acceptErr  error
	stopErr    error
	closeErr   error
}

func startWithOptions(
	socketPath string,
	mode fs.FileMode,
	service BatchService,
	options serverOptions,
) (_ *Server, err error) {
	if service == nil {
		return nil, fmt.Errorf("Projection Batch control service must not be nil")
	}
	if err := validateServerLimits(options.limits); err != nil {
		return nil, err
	}
	if options.readPeerIdentity == nil {
		return nil, fmt.Errorf("control peer identity reader must not be nil")
	}
	prepared, err := prepareSocketPath(socketPath, options.security)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.Close())
		}
	}()

	listener, err := prepared.listen()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, listener.Close())
		}
	}()
	if err := prepared.setMetadata(mode); err != nil {
		return nil, err
	}
	if err := prepared.grantRoleAccess(options.security); err != nil {
		return nil, err
	}
	if err := prepared.verifyConfiguredSocket(); err != nil {
		return nil, err
	}

	server := &Server{
		listener:   listener,
		prepared:   prepared,
		mode:       mode,
		service:    service,
		options:    options,
		stopping:   make(chan struct{}),
		acceptDone: make(chan struct{}),
		slots:      make(chan struct{}, options.limits.maxConnections),
	}
	go server.acceptLoop()
	if options.ready != nil {
		close(options.ready)
	}
	return server, nil
}

func serveStarted(ctx context.Context, server *Server) error {
	select {
	case <-ctx.Done():
	case <-server.Done():
	}
	return errors.Join(
		server.StopAccepting(),
		server.Wait(),
		server.Close(),
	)
}

func (server *Server) Done() <-chan struct{} {
	return server.acceptDone
}

func (server *Server) Err() error {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.acceptErr
}

func (server *Server) Check() error {
	if server == nil || server.prepared == nil {
		return errors.New("control socket runtime is unavailable")
	}
	select {
	case <-server.acceptDone:
		return errors.New("control socket is not accepting requests")
	default:
	}
	return server.prepared.verifyRuntimeMetadata(server.mode, server.options.security)
}

func (server *Server) StopAccepting() error {
	server.stopOnce.Do(func() {
		close(server.stopping)
		server.stopErr = server.listener.SetDeadline(time.Now())
	})
	<-server.acceptDone
	return errors.Join(server.stopErr, server.Err())
}

func (server *Server) Wait() error {
	server.handlers.Wait()
	return nil
}

func (server *Server) Close() error {
	server.closeOnce.Do(func() {
		server.closeErr = errors.Join(
			server.StopAccepting(),
			server.Wait(),
			server.listener.Close(),
			server.prepared.Close(),
		)
	})
	return server.closeErr
}

func (server *Server) acceptLoop() {
	defer close(server.acceptDone)
	for {
		select {
		case server.slots <- struct{}{}:
		case <-server.stopping:
			return
		}

		connection, err := server.listener.AcceptUnix()
		if err != nil {
			<-server.slots
			select {
			case <-server.stopping:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if retryableAcceptError(err) {
				timer := time.NewTimer(server.options.limits.acceptRetryDelay)
				select {
				case <-server.stopping:
					timer.Stop()
					return
				case <-timer.C:
					continue
				}
			}
			server.setAcceptError(fmt.Errorf(
				"accept ScrapeFS control connection: %w",
				err,
			))
			return
		}
		select {
		case <-server.stopping:
			connection.Close()
			<-server.slots
			return
		default:
		}
		server.handlers.Add(1)
		go func() {
			defer server.handlers.Done()
			defer func() {
				<-server.slots
			}()
			handleConnection(
				context.Background(),
				connection,
				server.service,
				server.options,
			)
		}()
	}
}

func (server *Server) setAcceptError(err error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.acceptErr = err
}
