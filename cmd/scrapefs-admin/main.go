package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/zendext/scrape-fs/internal/desktopadmin"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, input io.Reader, output io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("scrapefs-admin requires system authorization")
	}
	if len(arguments) == 2 && arguments[0] == "service" {
		action := arguments[1]
		if action != "start" && action != "stop" && action != "restart" {
			return fmt.Errorf("unsupported service action")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := exec.CommandContext(ctx, "/usr/bin/systemctl", action, "scrapefsd.service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", action, err, result)
		}
		return json.NewEncoder(output).Encode(struct{}{})
	}
	if len(arguments) != 1 {
		return fmt.Errorf("expected read-config, save-config, or service start|stop|restart")
	}
	switch arguments[0] {
	case "read-config":
		document, err := desktopadmin.ReadConfiguration()
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(document)
	case "save-config":
		var request struct {
			Text     string `json:"text"`
			Expected string `json:"expected"`
		}
		decoder := json.NewDecoder(io.LimitReader(input, desktopadmin.MaxConfigBytes*6+4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("expected a single request document")
		}
		document, err := desktopadmin.SaveConfiguration(request.Text, request.Expected)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(document)
	default:
		return fmt.Errorf("unsupported administration command")
	}
}
