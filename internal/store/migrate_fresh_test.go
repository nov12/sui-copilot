package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestFreshDatabaseMigrates(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("fresh database failed to open: %v", err)
	}
	defer db.Close()
	if _, err := db.Get(context.Background(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected get error: %v", err)
	}
}
