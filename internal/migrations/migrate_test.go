package migrations

import (
	"fmt"
	"io/fs"
	"testing"
)

func TestEmbeddedGatewayMigrationsRemainContiguous(t *testing.T) {
	entries, err := fs.ReadDir(files, "sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 20 {
		t.Fatalf("migration count = %d, want 20", len(entries))
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
