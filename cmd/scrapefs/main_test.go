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
	if !strings.Contains(stdout.String(), "Usage: scrapefs [options]") {
		t.Errorf("help output = %q, want scrapefs usage", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("help stderr = %q, want empty", stderr.String())
	}
}
