package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProgramHelp(t *testing.T) {
	t.Parallel()

	program := Program{
		Name:        "scrapefs",
		Description: "ScrapeFS control CLI",
		Version:     "test-version",
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := program.Run([]string{"--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("Run(--help) exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout.String(), "Usage: scrapefs [options]") {
		t.Errorf("help output %q does not contain usage", stdout.String())
	}
	if !strings.Contains(stdout.String(), program.Description) {
		t.Errorf("help output %q does not contain description", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("help stderr = %q, want empty", stderr.String())
	}
}

func TestProgramVersion(t *testing.T) {
	t.Parallel()

	program := Program{
		Name:        "scrapefsd",
		Description: "ScrapeFS daemon",
		Version:     "test-version",
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := program.Run([]string{"--version"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("Run(--version) exit code = %d, want 0", exitCode)
	}
	if got, want := stdout.String(), "scrapefsd test-version\n"; got != want {
		t.Errorf("version output = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("version stderr = %q, want empty", stderr.String())
	}
}

func TestProgramRejectsUnexpectedArguments(t *testing.T) {
	t.Parallel()

	program := Program{
		Name:        "scrapefs",
		Description: "ScrapeFS control CLI",
		Version:     "test-version",
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := program.Run([]string{"unexpected"}, &stdout, &stderr)

	if exitCode != 2 {
		t.Fatalf("Run(unexpected) exit code = %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Errorf("unexpected argument stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), `unexpected argument "unexpected"`) {
		t.Errorf("unexpected argument stderr = %q", stderr.String())
	}
}

func TestHelpAndVersionCreateNoFiles(t *testing.T) {
	program := Program{
		Name:        "scrapefs",
		Description: "ScrapeFS control CLI",
		Version:     "test-version",
	}

	tempDir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir(%q): %v", tempDir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	for _, args := range [][]string{{"--help"}, {"--version"}} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		if exitCode := program.Run(args, &stdout, &stderr); exitCode != 0 {
			t.Fatalf("Run(%v) exit code = %d, want 0", args, exitCode)
		}
	}

	var created []string
	err = filepath.WalkDir(tempDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != tempDir {
			created = append(created, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%q): %v", tempDir, err)
	}
	if len(created) != 0 {
		t.Errorf("help/version created files: %v", created)
	}
}
