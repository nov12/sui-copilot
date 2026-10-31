package service

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// systemdUnit is the unit file for the sui-copilot service. Restart and stop
// timeouts mirror the procd respawn 3600 5 5 / term_timeout 5 semantics.
const systemdUnit = `[Unit]
Description=sui-copilot accounting assistant
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory={{WORKDIR}}
ExecStart={{PROG}} {{ARGS}}
Restart=on-failure
RestartSec=5s
TimeoutStopSec=5s
UMask=0077

[Install]
WantedBy=multi-user.target
`

var (
	systemdUnitPath = "/etc/systemd/system/sui-copilot.service"
	systemdUnitName = "sui-copilot.service"
)

type systemdManager struct {
	run runner
}

func newSystemdManager() *systemdManager {
	return &systemdManager{run: execRunner{}}
}

func (m *systemdManager) Name() string { return "systemd" }

func (m *systemdManager) Install(opts Options) error {
	unit, err := renderSystemdUnit(opts)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(systemdUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", systemdUnitPath, err)
	}
	if _, err := m.run.Run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if _, err := m.run.Run("systemctl", "enable", systemdUnitName); err != nil {
		return fmt.Errorf("enable service: %w", err)
	}
	// Restart unconditionally so a repeated install also applies the new
	// definition to the running process; enable --now alone would leave an
	// already running service untouched.
	if _, err := m.run.Run("systemctl", "restart", systemdUnitName); err != nil {
		return fmt.Errorf("restart service: %w", err)
	}
	return nil
}

func (m *systemdManager) Uninstall() error {
	if _, err := m.run.Run("systemctl", "disable", "--now", systemdUnitName); err != nil {
		return fmt.Errorf("disable service: %w", err)
	}
	if err := os.Remove(systemdUnitPath); err != nil {
		return fmt.Errorf("remove %s: %w", systemdUnitPath, err)
	}
	if _, err := m.run.Run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	return nil
}

func (m *systemdManager) Start() error {
	if _, err := m.run.Run("systemctl", "start", systemdUnitName); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func (m *systemdManager) Stop() error {
	if _, err := m.run.Run("systemctl", "stop", systemdUnitName); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	return nil
}

func (m *systemdManager) Restart() error {
	if _, err := m.run.Run("systemctl", "restart", systemdUnitName); err != nil {
		return fmt.Errorf("restart service: %w", err)
	}
	return nil
}

func (m *systemdManager) Status() (string, error) {
	output, err := m.run.Run("systemctl", "status", systemdUnitName)
	return output, err
}

// installedProgramPath reads ExecStart from the installed unit file.
func (m *systemdManager) InstalledProgramPath() (string, error) {
	data, err := os.ReadFile(systemdUnitPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotInstalled
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", systemdUnitPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); found {
			return strings.Fields(value)[0], nil
		}
	}
	return "", fmt.Errorf("%s has no ExecStart definition", systemdUnitPath)
}

// renderSystemdUnit fills the unit template. ExecStart is not run through a
// shell, so no shell quoting applies; arguments only need systemd list syntax,
// which plain flag values satisfy because validation restricts embedded values
// to characters that need no escaping.
func renderSystemdUnit(opts Options) (string, error) {
	if !safePath(opts.Program) || !safePath(opts.WorkDir) {
		return "", fmt.Errorf("program and working directory must be absolute paths with safe characters (letters, digits, and ._-=:@[])")
	}
	args, err := renderSystemdArgs(opts.Args)
	if err != nil {
		return "", err
	}
	unit := systemdUnit
	unit = strings.ReplaceAll(unit, "{{PROG}}", opts.Program)
	unit = strings.ReplaceAll(unit, "{{WORKDIR}}", opts.WorkDir)
	unit = strings.ReplaceAll(unit, "{{ARGS}}", args)
	return unit, nil
}

// renderSystemdArgs validates each --flag=value argument for direct ExecStart
// embedding.
func renderSystemdArgs(args []string) (string, error) {
	for _, arg := range args {
		if err := validateArg(arg); err != nil {
			return "", err
		}
	}
	return strings.Join(args, " "), nil
}
