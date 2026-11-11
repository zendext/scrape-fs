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
