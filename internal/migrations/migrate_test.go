package migrations

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedGatewayMigrationsRemainContiguous(t *testing.T) {
	entries, err := fs.ReadDir(files, "sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 24 {
		t.Fatalf("migration count = %d, want 24", len(entries))
	}
	for index, entry := range entries {
		prefix := fmt.Sprintf("%06d_", index+1)
		if entry.IsDir() || len(entry.Name()) < len(prefix) || entry.Name()[:len(prefix)] != prefix {
			t.Fatalf("migration %d = %q, want prefix %q", index, entry.Name(), prefix)
		}
		payload, err := files.ReadFile("sql/" + entry.Name())
		if err != nil || len(payload) == 0 {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
	}
}

func TestFinalGatewayMigrationPublishesCurrentSchemaVersion(t *testing.T) {
	entries, err := fs.ReadDir(files, "sql")
	if err != nil {
		t.Fatal(err)
	}
	final := entries[len(entries)-1]
	payload, err := files.ReadFile("sql/" + final.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("current_version=%d", len(entries))
	if !strings.Contains(string(payload), want) {
		t.Fatalf("final migration %q does not publish %q", final.Name(), want)
	}
}
