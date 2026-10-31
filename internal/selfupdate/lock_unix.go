//go:build unix

package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// installLock is held for one whole update run — version decision, binary
// replacement, service restart, and marker cleanup — so concurrent updates
// cannot interleave those steps. The lock is an advisory flock on the binary's
// directory: that inode never changes when an update renames a new binary over
// the old one, so mutual exclusion holds across the replacement. No lock file
// is created, and the kernel releases the lock when the process exits.
type installLock struct {
	file *os.File
}

// TryInstallLock takes a non-blocking exclusive lock for updating the binary
// at executablePath. It returns a nil lock without error when another process
// already holds the lock.
func TryInstallLock(executablePath string) (*installLock, error) {
	file, err := os.Open(filepath.Dir(executablePath))
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return &installLock{file: file}, nil
	}
	file.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, nil
	}
	return nil, err
}

// Close releases the lock.
func (l *installLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}
