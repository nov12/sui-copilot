package service

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// The stat indirection exists so tests can inject a fake filesystem.
var stat = os.Stat

// procdScript is the OpenWrt init script rendered with the program path,
// working directory, and persisted arguments.
const procdScript = `#!/bin/sh /etc/rc.common

USE_PROCD=1

START=99
STOP=10

PROG="{{PROG}}"
WORKDIR="{{WORKDIR}}"
ARGS="{{ARGS}}"

start_service() {
    echo "Starting sui-copilot..."

    if [ ! -x "$PROG" ]; then
        echo "Error: $PROG not found or not executable"
        return 1
    fi

    procd_open_instance

    procd_set_param command /bin/sh -c "cd '$WORKDIR' && exec '$PROG' $ARGS"

    # 异常退出自动重启
    procd_set_param respawn 3600 5 5

    # 停止时先发送 TERM，等待 5 秒
    procd_set_param term_timeout 5

    # 输出到 logread
    procd_set_param stdout 1
    procd_set_param stderr 1

    procd_close_instance
}

service_started() {
    echo "sui-copilot started."
}

stop_service() {
    echo "Stopping sui-copilot..."
}

service_stopped() {
    echo "sui-copilot stopped."
}
`

var (
	procdScriptPath = "/etc/init.d/sui-copilot"
)

type procdManager struct {
	run runner
}

func newProcdManager() *procdManager {
	return &procdManager{run: execRunner{}}
}

func (m *procdManager) Name() string { return "procd" }

func (m *procdManager) Install(opts Options) error {
	script, err := renderProcdScript(opts)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(procdScriptPath, []byte(script), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", procdScriptPath, err)
	}
	if _, err := m.run.Run(procdScriptPath, "enable"); err != nil {
		return fmt.Errorf("enable service: %w", err)
	}
	if _, err := m.run.Run(procdScriptPath, "restart"); err != nil {
		return fmt.Errorf("restart service: %w", err)
	}
	return nil
}

func (m *procdManager) Uninstall() error {
	if _, err := m.run.Run(procdScriptPath, "stop"); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if _, err := m.run.Run(procdScriptPath, "disable"); err != nil {
		return fmt.Errorf("disable service: %w", err)
	}
	if err := os.Remove(procdScriptPath); err != nil {
		return fmt.Errorf("remove %s: %w", procdScriptPath, err)
	}
	return nil
}

func (m *procdManager) Start() error {
	if _, err := m.run.Run(procdScriptPath, "start"); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func (m *procdManager) Stop() error {
	if _, err := m.run.Run(procdScriptPath, "stop"); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	return nil
}

func (m *procdManager) Restart() error {
	if _, err := m.run.Run(procdScriptPath, "restart"); err != nil {
		return fmt.Errorf("restart service: %w", err)
	}
	return nil
}

func (m *procdManager) Status() (string, error) {
	output, err := m.run.Run(procdScriptPath, "status")
	return output, err
}

// installedProgramPath reads PROG from the installed init script.
func (m *procdManager) InstalledProgramPath() (string, error) {
	data, err := os.ReadFile(procdScriptPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotInstalled
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", procdScriptPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "PROG="); found {
			return strings.Trim(value, `"`), nil
		}
	}
	return "", fmt.Errorf("%s has no PROG definition", procdScriptPath)
}

// renderProcdScript fills the template and rejects values that could break out
// of the generated sh -c command.
func renderProcdScript(opts Options) (string, error) {
	if !safePath(opts.Program) || !safePath(opts.WorkDir) {
		return "", fmt.Errorf("program and working directory must be absolute paths with safe characters (letters, digits, and ._-=:@[])")
	}
	args, err := renderProcdArgs(opts.Args)
	if err != nil {
		return "", err
	}
	script := procdScript
	script = strings.ReplaceAll(script, "{{PROG}}", opts.Program)
	script = strings.ReplaceAll(script, "{{WORKDIR}}", opts.WorkDir)
	script = strings.ReplaceAll(script, "{{ARGS}}", args)
	return script, nil
}

// renderProcdArgs quotes each argument for the sh -c command. Values arrive
// already in --flag=value form.
func renderProcdArgs(args []string) (string, error) {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if err := validateArg(arg); err != nil {
			return "", err
		}
		quoted = append(quoted, quoteArg(arg))
	}
	return strings.Join(quoted, " "), nil
}
