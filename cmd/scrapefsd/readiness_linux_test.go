package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifyReadyUsesOptionalSystemdSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifyReady(); err != nil {
		t.Fatalf("notifyReady without socket: %v", err)
	}

	socketPath := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram(
		"unixgram",
		&net.UnixAddr{Name: socketPath, Net: "unixgram"},
	)
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", socketPath)

	message := make(chan string, 1)
	go func() {
		buffer := make([]byte, 64)
		if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			message <- "deadline error: " + err.Error()
			return
		}
		read, _, err := listener.ReadFromUnix(buffer)
		if err != nil {
			message <- "read error: " + err.Error()
			return
		}
		message <- string(buffer[:read])
	}()
	if err := notifyReady(); err != nil {
		t.Fatalf("notifyReady: %v", err)
	}
	select {
	case got := <-message:
		if got != "READY=1" {
			t.Errorf("readiness message = %q, want READY=1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness message was not received")
	}
}

func TestNotifyReadySupportsAbstractSystemdSocket(t *testing.T) {
	socketName := fmt.Sprintf(
		"scrapefsd-ready-%d-%d",
		os.Getpid(),
		time.Now().UnixNano(),
	)
	listener, err := net.ListenUnixgram(
		"unixgram",
		&net.UnixAddr{Name: "\x00" + socketName, Net: "unixgram"},
	)
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", "@"+socketName)

	if err := notifyReady(); err != nil {
		t.Fatalf("notifyReady: %v", err)
	}
	buffer := make([]byte, 64)
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	read, _, err := listener.ReadFromUnix(buffer)
	if err != nil {
		t.Fatalf("ReadFromUnix: %v", err)
	}
	if got := string(buffer[:read]); got != "READY=1" {
		t.Errorf("readiness message = %q, want READY=1", got)
	}
}
