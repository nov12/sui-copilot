//go:build !unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestServiceRestartClearsMarkerNoLock covers the no-lock platforms (see
// lock_other.go): the manual restart cannot be refused — the lock is a no-op —
// so the only contract is that a successful restart clears the pending marker.
func TestServiceRestartClearsMarkerNoLock(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	if err := os.WriteFile(markerPath(dir), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{programPath: filepath.Join(dir, assetName)}
	withHooks(t, manager, restartServiceAfterUpdate)

	if err := runService([]string{"restart"}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if manager.restartCalls != 1 {
		t.Errorf("restart calls = %d, want 1", manager.restartCalls)
	}
	if _, err := os.Stat(markerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("marker should be cleared after a successful manual restart")
	}
}
