package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/zendext/scrape-fs/internal/diagnostics"
)

func TestDiagnosticsClientServerRoundTrip(t *testing.T) {
	socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
	service := &recordingDiagnosticsService{
		status: diagnostics.StatusResult{
			Schema: diagnostics.StatusSchema,
			State:  diagnostics.StateReady,
			Daemon: diagnostics.DaemonStatus{Version: "1.2.3", Ready: true},
		},
		doctor: diagnostics.DoctorResult{
			Schema:   diagnostics.DoctorSchema,
			Severity: diagnostics.SeverityWarning,
			Findings: []diagnostics.Finding{
				{
					Severity:  diagnostics.SeverityWarning,
					Code:      diagnostics.CodeOrphanObject,
					Component: "object_store",
					Message:   "orphan",
					ObjectID:  42,
				},
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serveWithOptions(
			ctx,
			socketPath,
			0o660,
			staticBatchService{},
			serverOptions{
				limits: defaultServerLimits(),
				security: serverSecurity{
					daemonUID:        uint32(os.Geteuid()),
					administratorUID: uint32(os.Geteuid()),
					controlGroupID:   uint32(os.Getegid()),
				},
				readPeerIdentity: func(*net.UnixConn) (peerIdentity, error) {
					return peerIdentity{
						UID: differentUint32(uint32(os.Geteuid())),
						GID: uint32(os.Getegid()),
					}, nil
				},
				ready:              ready,
				diagnosticsService: service,
			},
		)
	}()
	waitForServerReady(t, ready)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	client := NewClient(socketPath)
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !reflect.DeepEqual(status, service.status) {
		t.Errorf("Status = %#v, want %#v", status, service.status)
	}
	doctor, err := client.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !reflect.DeepEqual(doctor, service.doctor) {
		t.Errorf("Doctor = %#v, want %#v", doctor, service.doctor)
	}
	if statusCalls, doctorCalls := service.Calls(); statusCalls != 1 || doctorCalls != 1 {
		t.Errorf("diagnostics calls = status %d doctor %d, want 1/1", statusCalls, doctorCalls)
	}
}

func TestDiagnosticsRejectUnauthorizedCallerBeforeService(t *testing.T) {
	for _, command := range []string{"status", "doctor"} {
		t.Run(command, func(t *testing.T) {
			socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
			service := &recordingDiagnosticsService{}
			ctx, cancel := context.WithCancel(context.Background())
			ready := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- serveWithOptions(
					ctx,
					socketPath,
					0o660,
					staticBatchService{},
					serverOptions{
						limits: defaultServerLimits(),
						security: serverSecurity{
							daemonUID:        uint32(os.Geteuid()),
							administratorUID: uint32(os.Geteuid()),
							controlGroupID:   uint32(os.Getegid()),
						},
						readPeerIdentity: func(*net.UnixConn) (peerIdentity, error) {
							return peerIdentity{
								UID: differentUint32(uint32(os.Geteuid())),
								GID: differentUint32(uint32(os.Getegid())),
							}, nil
						},
						ready:              ready,
						diagnosticsService: service,
					},
				)
			}()
			waitForServerReady(t, ready)
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("Serve: %v", err)
				}
			})

			client := NewClient(socketPath)
			var err error
			if command == "status" {
				_, err = client.Status(context.Background())
			} else {
				_, err = client.Doctor(context.Background())
			}
			var remote *RemoteError
			if !errors.As(err, &remote) || remote.Code != "PermissionDenied" {
				t.Fatalf("%s error = %v, want PermissionDenied", command, err)
			}
			if statusCalls, doctorCalls := service.Calls(); statusCalls != 0 || doctorCalls != 0 {
				t.Errorf(
					"diagnostics calls = status %d doctor %d, want 0/0",
					statusCalls,
					doctorCalls,
				)
			}
		})
	}
}

func TestServerCheckRejectsConfiguredSocketReplacement(t *testing.T) {
	socketPath := filepath.Join(controlSocketTempDir(t), "control.sock")
	server, err := startWithOptions(
		socketPath,
		0o660,
		staticBatchService{},
		serverOptions{
			limits: defaultServerLimits(),
			security: serverSecurity{
				daemonUID:        uint32(os.Geteuid()),
				administratorUID: uint32(os.Geteuid()),
				controlGroupID:   uint32(os.Getegid()),
			},
			readPeerIdentity: readPeerIdentity,
		},
	)
	if err != nil {
		t.Fatalf("startWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := server.Check(); err != nil {
		t.Fatalf("healthy Check: %v", err)
	}

	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove configured socket: %v", err)
	}
	if err := os.WriteFile(socketPath, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := server.Check(); err == nil {
		t.Fatal("Check after socket replacement succeeded")
	}
}

type recordingDiagnosticsService struct {
	mu          sync.Mutex
	statusCalls int
	doctorCalls int
	status      diagnostics.StatusResult
	doctor      diagnostics.DoctorResult
}

func (service *recordingDiagnosticsService) Status(context.Context) diagnostics.StatusResult {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.statusCalls++
	return service.status
}

func (service *recordingDiagnosticsService) Doctor(context.Context) diagnostics.DoctorResult {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.doctorCalls++
	return service.doctor
}

func (service *recordingDiagnosticsService) Calls() (int, int) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.statusCalls, service.doctorCalls
}
