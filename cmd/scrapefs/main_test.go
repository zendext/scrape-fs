package main

import (
	"bytes"
	"strings"
	"testing"
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
