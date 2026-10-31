//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sui-copilot/internal/selfupdate"
)

// TestServiceRestartRespectsUpdateLock covers the Unix advisory-lock contract:
// while an update holds the directory lock, a manual restart is refused and
// leaves the pending-restart marker in place. On platforms without the lock
// (see lock_other.go) the lock is a no-op, so this contract has no meaning
// there; restart_restart_clears_marker.go covers what those platforms do.
func TestServiceRestartRespectsUpdateLock(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	if err := os.WriteFile(markerPath(dir), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{programPath: filepath.Join(dir, assetName)}
	withHooks(t, manager, restartServiceAfterUpdate)

	// Simulate a running update by holding the lock, then try a manual
	// restart: it must be refused and must not clear the pending marker.
	lock, err := selfupdate.TryInstallLock(filepath.Join(dir, assetName))
	if err != nil {
		t.Fatalf("take update lock: %v", err)
	}
	if lock == nil {
		t.Fatal("lock should be acquirable before the service command runs")
	}
	err = runService([]string{"restart"})
	lock.Close()
	if err == nil || !strings.Contains(err.Error(), "另一个 update 进程正在进行") {
		t.Fatalf("want lock conflict error, got %v", err)
	}
	if manager.restartCalls != 0 {
		t.Errorf("restart calls = %d, want 0 while an update holds the lock", manager.restartCalls)
	}
	if _, err := os.Stat(markerPath(dir)); err != nil {
		t.Errorf("pending marker must survive a refused restart, stat err = %v", err)
	}

	// Without the lock held, the restart succeeds and clears the marker.
	if err := runService([]string{"restart"}); err != nil {
		t.Fatalf("uncontended restart: %v", err)
	}
	if manager.restartCalls != 1 {
		t.Errorf("restart calls = %d, want 1", manager.restartCalls)
	}
	if _, err := os.Stat(markerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("marker should be cleared after a successful manual restart")
	}
}
