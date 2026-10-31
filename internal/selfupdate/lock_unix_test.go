//go:build unix

package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryInstallLock(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "sui-copilot")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := TryInstallLock(executable)
	if err != nil {
		t.Fatalf("first TryInstallLock: %v", err)
	}
	if first == nil {
		t.Fatal("first lock should be acquired")
	}

	second, err := TryInstallLock(executable)
	if err != nil {
		t.Fatalf("second TryInstallLock: %v", err)
	}
	if second != nil {
		second.Close()
		t.Fatal("second lock should be refused while the first is held")
	}

	// Replacing the binary under the lock must not weaken mutual exclusion:
	// the lock is on the directory, whose inode does not change.
	newBinary := filepath.Join(dir, "staged")
	if err := os.WriteFile(newBinary, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newBinary, executable); err != nil {
		t.Fatal(err)
	}
	third, err := TryInstallLock(executable)
	if err != nil {
		t.Fatalf("TryInstallLock after replacement: %v", err)
	}
	if third != nil {
		third.Close()
		t.Fatal("lock must survive a binary replacement")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	fourth, err := TryInstallLock(executable)
	if err != nil {
		t.Fatalf("TryInstallLock after release: %v", err)
	}
	if fourth == nil {
		t.Fatal("lock should be acquirable after release")
	}
	fourth.Close()

	// No lock file is left behind: locking needs no write access and a
	// root-owned lock can never block other users.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "sui-copilot" {
			t.Errorf("locking should not create files, found %q", entry.Name())
		}
	}
}

func TestTryInstallLockMissingDirectory(t *testing.T) {
	if _, err := TryInstallLock(filepath.Join(t.TempDir(), "missing-dir", "sui-copilot")); err == nil {
		t.Error("lock on a missing directory should fail")
	}
}
