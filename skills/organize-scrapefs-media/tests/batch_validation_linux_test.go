//go:build linux

package skilltest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zendext/scrape-fs/internal/database"
	"github.com/zendext/scrape-fs/internal/library"
	"github.com/zendext/scrape-fs/internal/namespace"
	"github.com/zendext/scrape-fs/internal/projectionbatch"
)

func TestGeneratedProfileBatchesPassRealParserAndValidation(t *testing.T) {
	for _, profile := range []string{"jellyfin", "plex"} {
		t.Run(profile, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "movie")
			copyFixture(t, filepath.Join("fixtures", "movie-directory"), source)
			batchPath := filepath.Join(root, "batch.json")
			artifactsPath := filepath.Join(root, "artifacts.json")
			command := exec.Command(
				"python3",
				filepath.Join("..", "scripts", "scrapefs_media.py"),
				"plan",
				"--source", source,
				"--library", "media",
				"--profile", profile,
				"--batch", batchPath,
				"--artifacts", artifactsPath,
				"--ffprobe", filepath.Join("fixtures", "bin", "ffprobe"),
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("plan %s batch: %v\n%s", profile, err, output)
			}

			data, err := os.ReadFile(batchPath)
			if err != nil {
				t.Fatalf("read generated batch: %v", err)
			}
			if _, err := projectionbatch.Parse(data); err != nil {
				t.Fatalf("Parse generated %s batch: %v", profile, err)
			}

			databaseDirectory := filepath.Join(root, "database")
			if err := os.Mkdir(databaseDirectory, 0o700); err != nil {
				t.Fatalf("create database directory: %v", err)
			}
			opened, err := database.OpenWithOptions(
				context.Background(),
				filepath.Join(databaseDirectory, "scrapefs.db"),
				database.Options{LockPath: filepath.Join(root, "scrapefsd.lock")},
			)
			if err != nil {
				t.Fatalf("open database: %v", err)
			}
			t.Cleanup(func() {
				if err := opened.Close(); err != nil {
					t.Errorf("close database: %v", err)
				}
			})
			repository := namespace.NewRepository(opened)
			if _, err := library.NewService(repository).Create(
				context.Background(),
				library.CreateRequest{
					Name:    "media",
					Profile: library.ProfileIdentifier{Name: profile, Version: 1},
					Actor:   namespace.Actor{Kind: namespace.ActorSystem},
					UID:     uint32(os.Geteuid()),
					GID:     uint32(os.Getegid()),
					Modes:   library.DefaultModes,
				},
			); err != nil {
				t.Fatalf("create %s Library: %v", profile, err)
			}
			validated, err := projectionbatch.NewService(repository).Validate(
				context.Background(),
				data,
			)
			if err != nil {
				t.Fatalf("Validate generated %s batch: %v", profile, err)
			}
			if !validated.Valid || len(validated.Entries) != 2 {
				t.Fatalf("validation result = %#v, want two valid entries", validated)
			}
		})
	}
}

func TestGeneratedBatchHasOnlyStrictSchemaFields(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "episodes")
	copyFixture(t, filepath.Join("fixtures", "episodes"), source)
	batchPath := filepath.Join(root, "batch.json")
	command := exec.Command(
		"python3",
		filepath.Join("..", "scripts", "scrapefs_media.py"),
		"plan",
		"--source", source,
		"--library", "media",
		"--profile", "jellyfin",
		"--batch", batchPath,
		"--artifacts", filepath.Join(root, "artifacts.json"),
		"--ffprobe", filepath.Join("fixtures", "bin", "ffprobe"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("plan episode batch: %v\n%s", err, output)
	}

	data, err := os.ReadFile(batchPath)
	if err != nil {
		t.Fatalf("read generated batch: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode generated batch: %v", err)
	}
	if len(document) != 3 {
		t.Fatalf("top-level fields = %#v, want strict schema/library/entries", document)
	}
	if _, err := projectionbatch.Parse(data); err != nil {
		t.Fatalf("Parse generated episode batch: %v", err)
	}
}

func copyFixture(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
		t.Fatalf("copy fixture %q: %v", source, err)
	}
}
