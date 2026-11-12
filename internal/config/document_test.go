package config

import (
	"strings"
	"testing"
)

func TestPatchDocumentPreservesCommentsAndHandlesMissingFields(t *testing.T) {
	input := "# keep this\n[fuse]\nmountpoint = '/mnt/old' # mount note\n\n[control]\nadministrator_uid = 1000\n"
	patched, err := PatchDocument(input, map[string]string{
		"fuse.mountpoint":        "/mnt/new directory",
		"fuse.entry_timeout":     "5s",
		"control.socket_mode":    "0o660",
		"gc.orphan_grace_period": "48h",
		"storage.database_path":  "/var/lib/scrapefs/new.db",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patched.Text, "# keep this") || !strings.Contains(patched.Text, "# mount note") {
		t.Fatalf("comments lost: %s", patched.Text)
	}
	if strings.Count(patched.Text, "[fuse]") != 1 || patched.Fields["fuse.mountpoint"] != "/mnt/new directory" || patched.Fields["gc.orphan_grace_period"] != "48h0m0s" {
		t.Fatalf("unexpected patch: %+v", patched)
	}
	unchanged, err := PatchDocument(patched.Text, patched.Fields)
	if err != nil || unchanged.Text != patched.Text {
		t.Fatalf("unchanged fields rewrote the file: %v", err)
	}
}

func TestPatchDocumentRejectsUnknownInvalidAndOverlappingValues(t *testing.T) {
	for _, fields := range []map[string]string{
		{"agent.provider": "unexpected"},
		{"fuse.mountpoint": "relative"},
		{"storage.database_path": "/mnt/scrapefs/state.db"},
		{"control.socket_mode": "0o666; invalid"},
	} {
		if _, err := PatchDocument("", fields); err == nil {
			t.Fatalf("accepted invalid fields: %v", fields)
		}
	}
}

func TestPatchDocumentEditsInlineTablesAndDottedKeys(t *testing.T) {
	for _, input := range []string{
		"fuse = { attr_timeout = '2s', entry_timeout = '4s' } # keep inline\n",
		"fuse = {} # keep empty\n[control]\nadministrator_uid = 1000\n",
		"fuse.attr_timeout = '2s' # keep dotted\n[control]\nadministrator_uid = 1000\n",
		"fuse.mountpoint = '/mnt/scrapefs' # keep missing\n",
		"fuse = {attr_timeout = '2s'}",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseDocument(input); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			patched, err := PatchDocument(input, map[string]string{
				"fuse.attr_timeout":     "3s",
				"fuse.negative_timeout": "1s",
			})
			if err != nil {
				t.Fatal(err)
			}
			if patched.Fields["fuse.attr_timeout"] != "3s" || patched.Fields["fuse.negative_timeout"] != "1s" {
				t.Fatalf("unexpected fields: %+v", patched.Fields)
			}
			for _, retained := range []string{"# keep inline", "# keep empty", "# keep dotted", "# keep missing", "entry_timeout = '4s'", "administrator_uid = 1000", "mountpoint = '/mnt/scrapefs'"} {
				if strings.Contains(input, retained) && !strings.Contains(patched.Text, retained) {
					t.Fatalf("changed unrelated text %q: %s", retained, patched.Text)
				}
			}
			unchanged, err := PatchDocument(patched.Text, patched.Fields)
			if err != nil || unchanged.Text != patched.Text {
				t.Fatalf("unchanged fields rewrote the file: %v", err)
			}
		})
	}
}
