//go:build !unix

package selfupdate

// installLock is a no-op on platforms without advisory file locks; replacing a
// running binary is not supported there anyway.
type installLock struct{}

// TryInstallLock always succeeds on platforms without advisory locks.
func TryInstallLock(executablePath string) (*installLock, error) {
	return &installLock{}, nil
}

// Close releases the no-op lock.
func (l *installLock) Close() error {
	return nil
}
