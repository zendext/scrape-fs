package desktopadmin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zendext/scrape-fs/internal/config"
)

func TestSaveConfigurationPreservesMetadataBackupAndRejectsStaleWrites(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := "# administrator comment\n[fuse]\nmountpoint = '/mnt/original'\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	store := configStore{directory: directory, owner: uint32(os.Geteuid())}
	old, _ := config.ParseDocument(original)
	updated, err := config.PatchDocument(original, map[string]string{"fuse.mountpoint": "/mnt/updated"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.save(updated.Text, old.Digest); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(filepath.Join(directory, "config.toml.previous"))
	if err != nil || string(backup) != original {
		t.Fatalf("backup = %q, %v", backup, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed: %v", info.Mode())
	}
	if _, err := store.save(original, old.Digest); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale edit accepted: %v", err)
	}
	if _, err := store.save("[agent]\nprovider='invalid'", updated.Digest); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	current, _ := os.ReadFile(path)
	if string(current) != updated.Text {
		t.Fatal("failed save changed the configuration")
	}
}

func TestSaveConfigurationRejectsSymlinksAndWritableDirectories(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.toml")
	os.WriteFile(outside, []byte(""), 0o600)
	os.Symlink(outside, filepath.Join(directory, "config.toml"))
	store := configStore{directory: directory, owner: uint32(os.Geteuid())}
	document, _ := config.ParseDocument("")
	if _, err := store.save("", document.Digest); err == nil {
		t.Fatal("symlink target accepted")
	}
	os.Remove(filepath.Join(directory, "config.toml"))
	os.WriteFile(filepath.Join(directory, "config.toml"), []byte(""), 0o600)
	os.Chmod(directory, 0o777)
	if _, err := store.save("", document.Digest); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("writable configuration directory accepted: %v", err)
	}
}

func TestSaveConfigurationRepairsInvalidOriginalAndBacksItUp(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.toml")
	original := "[invalid"
	os.WriteFile(path, []byte(original), 0o600)
	store := configStore{directory: directory, owner: uint32(os.Geteuid())}
	root, err := store.open()
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := store.read(root)
	root.Close()
	if err != nil || document.ValidationError == "" {
		t.Fatalf("invalid original unavailable for repair: %+v %v", document, err)
	}
	if _, err := store.save("# repaired\n", document.Digest); err != nil {
		t.Fatal(err)
	}
	backup, _ := os.ReadFile(path + ".previous")
	if string(backup) != original {
		t.Fatal("invalid original was not preserved")
	}
}
