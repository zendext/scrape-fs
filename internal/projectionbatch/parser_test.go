package projectionbatch

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsStrictBatchAndComputesCanonicalDigest(t *testing.T) {
	first := []byte(`{
		"schema": "scrapefs.projection-batch/v1",
		"library": "media",
		"entries": [
			{"source": "/srv/media/Dune.mkv", "target": "Movies/Dune/Dune.mkv"},
			{"source": "/srv/media/Dune.ass", "target": "Movies/Dune/Dune.ass"}
		]
	}`)
	second := []byte(`{"entries":[
		{"target":"Movies/Dune/Dune.mkv","source":"/srv/media/Dune.mkv"},
		{"target":"Movies/Dune/Dune.ass","source":"/srv/media/Dune.ass"}
	],"library":"media","schema":"scrapefs.projection-batch/v1"}`)

	firstBatch, err := Parse(first)
	if err != nil {
		t.Fatalf("Parse first: %v", err)
	}
	secondBatch, err := Parse(second)
	if err != nil {
		t.Fatalf("Parse second: %v", err)
	}

	if firstBatch.Digest != secondBatch.Digest {
		t.Errorf("equivalent digests differ: %q != %q", firstBatch.Digest, secondBatch.Digest)
	}
	if !strings.HasPrefix(firstBatch.Digest, "sha256:") || len(firstBatch.Digest) != 71 {
		t.Errorf("digest = %q, want sha256 plus 64 lowercase hex characters", firstBatch.Digest)
	}
	if firstBatch.Document.Entries[0].Source != "/srv/media/Dune.mkv" ||
		firstBatch.Document.Entries[1].Source != "/srv/media/Dune.ass" {
		t.Errorf("entry order changed: %#v", firstBatch.Document.Entries)
	}

	reversed, err := Parse([]byte(`{
		"schema":"scrapefs.projection-batch/v1",
		"library":"media",
		"entries":[
			{"source":"/srv/media/Dune.ass","target":"Movies/Dune/Dune.ass"},
			{"source":"/srv/media/Dune.mkv","target":"Movies/Dune/Dune.mkv"}
		]
	}`))
	if err != nil {
		t.Fatalf("Parse reversed: %v", err)
	}
	if reversed.Digest == firstBatch.Digest {
		t.Error("digest did not preserve entries array order")
	}
}

func TestParseRejectsInvalidBatchDocuments(t *testing.T) {
	validEntry := `{"source":"/srv/media/Dune.mkv","target":"Movies/Dune/Dune.mkv"}`
	tests := map[string]string{
		"invalid UTF-8":                string([]byte{'{', '"', 0xff, '"', ':', '1', '}'}),
		"trailing document":            `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[` + validEntry + `]} {}`,
		"unknown top-level field":      `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[` + validEntry + `],"extra":true}`,
		"unknown entry field":          `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"/srv/media/Dune.mkv","target":"Movies/Dune.mkv","extra":true}]}`,
		"duplicate top-level field":    `{"schema":"scrapefs.projection-batch/v1","schema":"scrapefs.projection-batch/v1","library":"media","entries":[` + validEntry + `]}`,
		"duplicate entry field":        `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"/srv/media/Dune.mkv","source":"/srv/media/Other.mkv","target":"Movies/Dune.mkv"}]}`,
		"invalid schema":               `{"schema":"scrapefs.projection-batch/v2","library":"media","entries":[` + validEntry + `]}`,
		"invalid library type":         `{"schema":"scrapefs.projection-batch/v1","library":1,"entries":[` + validEntry + `]}`,
		"invalid library name":         `{"schema":"scrapefs.projection-batch/v1","library":"media/library","entries":[` + validEntry + `]}`,
		"empty entries":                `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[]}`,
		"duplicate source":             `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[` + validEntry + `,{"source":"/srv/media/Dune.mkv","target":"Movies/Dune/Dune-copy.mkv"}]}`,
		"duplicate target":             `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[` + validEntry + `,{"source":"/srv/media/Other.mkv","target":"Movies/Dune/Dune.mkv"}]}`,
		"target is another target dir": `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"/srv/media/Dune.mkv","target":"Movies/Dune"},{"source":"/srv/media/Dune.ass","target":"Movies/Dune/Dune.ass"}]}`,
		"relative source":              `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"Dune.mkv","target":"Movies/Dune.mkv"}]}`,
		"non-normalized source":        `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"/srv/media/../Dune.mkv","target":"Movies/Dune.mkv"}]}`,
		"absolute target":              `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"/srv/media/Dune.mkv","target":"/Movies/Dune.mkv"}]}`,
		"non-normalized target":        `{"schema":"scrapefs.projection-batch/v1","library":"media","entries":[{"source":"/srv/media/Dune.mkv","target":"Movies//Dune.mkv"}]}`,
	}

	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(input))
			if !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("Parse error = %v, want ErrInvalidBatch", err)
			}
		})
	}
}
