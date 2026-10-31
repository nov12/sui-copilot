// Package service installs and controls the sui-copilot system service on
// supported Linux init systems (OpenWrt procd and systemd).
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options describes how to run the service. Paths must already be absolute.
type Options struct {
	Program string   // absolute path to the executable
	WorkDir string   // absolute working directory for the process
	Args    []string // explicit flags to persist, e.g. --config=... --addr=...
}

// Manager controls the service through one init backend.
type Manager interface {
	// Name identifies the backend, e.g. "procd" or "systemd".
	Name() string
	Install(opts Options) error
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	Status() (string, error)
}

// Detect returns a manager for the running system, or nil when no supported
// init backend is available.
func Detect() Manager {
	if detectProcd() {
		return newProcdManager()
	}
	if detectSystemd() {
		return newSystemdManager()
	}
	return nil
}

// ErrNotInstalled reports that the sui-copilot service is not registered.
var ErrNotInstalled = errors.New("sui-copilot service is not installed")

// ProgramPathReader is implemented by managers that can report the program
// path recorded in their registered service definition. The selfupdate flow
// uses it to verify that a service points at the binary it replaced, and
// command-level tests substitute fake managers for it.
type ProgramPathReader interface {
	InstalledProgramPath() (string, error)
}

// InstalledProgramPath reports the program path recorded in the registered
// service definition, or ErrNotInstalled when absent.
func InstalledProgramPath(m Manager) (string, error) {
	reader, ok := m.(ProgramPathReader)
	if !ok {
		return "", fmt.Errorf("backend %s cannot report the installed program path", m.Name())
	}
	return reader.InstalledProgramPath()
}

// detectProcd reports whether the system runs OpenWrt procd.
func detectProcd() bool {
	if !fileExists("/etc/rc.common") {
		return false
	}
	return fileExists("/sbin/procd") || fileExists("/bin/procd")
}

// detectSystemd reports whether the system boots with systemd.
func detectSystemd() bool {
	return dirExists("/run/systemd/system")
}

// writeFileAtomic writes data to path via a same-directory temp file and one
// rename, so an interrupted write never leaves a truncated service definition.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".sui-copilot-def-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, mode); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// runner executes commands so tests can capture the sequence.
type runner interface {
	Run(name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// quoteArg renders one argument for embedding in the generated procd script's
// sh -c command. The value must already pass validateArg.
func quoteArg(value string) string {
	return "'" + value + "'"
}

// safeValue reports whether s consists only of characters that can be embedded
// directly into both the generated procd script and the systemd unit without
// quoting or escaping: letters, digits, and ._ - / : = @ [ ].
func safeValue(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._-/:=@[]", r):
		default:
			return false
		}
	}
	return true
}

// safePath reports whether an absolute path can be embedded into the generated
// service definition.
func safePath(path string) bool {
	return filepath.IsAbs(path) && safeValue(path)
}

// validateArg checks one --flag=value argument before it is persisted into a
// service definition.
func validateArg(arg string) error {
	name, value, found := strings.Cut(arg, "=")
	if !found || !strings.HasPrefix(name, "-") || name == "-" || !safeValue(value) {
		return fmt.Errorf("service argument %q must use --flag=value form with safe characters (letters, digits, and ._-=:@[])", arg)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := stat(path)
	return err == nil && info.IsDir()
}
