package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

func notifyReady() error {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return nil
	}
	if strings.HasPrefix(socketPath, "@") {
		socketPath = "\x00" + strings.TrimPrefix(socketPath, "@")
	}
	connection, err := net.DialUnix(
		"unixgram",
		nil,
		&net.UnixAddr{Name: socketPath, Net: "unixgram"},
	)
	if err != nil {
		return fmt.Errorf("connect systemd notification socket: %w", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("send systemd readiness notification: %w", err)
	}
	return nil
}
