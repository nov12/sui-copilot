package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	commands []string
	failOn   map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{failOn: map[string]error{}}
}

func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	command := strings.Join(append([]string{name}, args...), " ")
	f.commands = append(f.commands, command)
	if err, ok := f.failOn[command]; ok {
		return "", err
	}
	return "", nil
}

func (f *fakeRunner) has(command string) bool {
	for _, existing := range f.commands {
		if existing == command {
			return true
		}
	}
	return false
}

func testOptions() Options {
	return Options{
		Program: "/opt/sui-copilot/sui-copilot-linux-amd64",
		WorkDir: "/opt/sui-copilot",
		Args:    []string{"--config=/opt/sui-copilot/sui-copilot.toml", "--addr=127.0.0.1:8080"},
	}
}

func TestDetectProcd(t *testing.T) {
	origStat := stat
	t.Cleanup(func() { stat = origStat })

	stat = func(path string) (os.FileInfo, error) {
		if path == "/etc/rc.common" || path == "/sbin/procd" {
			return fakeFileInfo{path, false}, nil
		}
		return nil, os.ErrNotExist
	}
	manager := Detect()
	if manager == nil || manager.Name() != "procd" {
		t.Fatalf("want procd manager, got %#v", manager)
	}

	stat = func(path string) (os.FileInfo, error) {
		if path == "/etc/rc.common" {
			return fakeFileInfo{path, false}, nil
		}
		return nil, os.ErrNotExist
	}
	if manager := Detect(); manager != nil {
		t.Fatalf("want nil without procd binary, got %#v", manager)
	}

	stat = func(path string) (os.FileInfo, error) {
		if path == "/run/systemd/system" {
			return fakeFileInfo{path, true}, nil
		}
		return nil, os.ErrNotExist
	}
	if manager := Detect(); manager == nil || manager.Name() != "systemd" {
		t.Fatalf("want systemd manager, got %#v", manager)
	}

	stat = func(path string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	}
	if manager := Detect(); manager != nil {
		t.Fatalf("want nil without any init backend, got %#v", manager)
	}
}

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return os.ModePerm }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

func TestRenderProcdScript(t *testing.T) {
	script, err := renderProcdScript(testOptions())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		`PROG="/opt/sui-copilot/sui-copilot-linux-amd64"`,
		`WORKDIR="/opt/sui-copilot"`,
		`ARGS="'--config=/opt/sui-copilot/sui-copilot.toml' '--addr=127.0.0.1:8080'"`,
		`procd_set_param command /bin/sh -c "cd '$WORKDIR' && exec '$PROG' $ARGS"`,
		"respawn 3600 5 5",
		"term_timeout 5",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n%s", want, script)
		}
	}
}

func TestRenderProcdScriptRejectsUnsafe(t *testing.T) {
	cases := []Options{
		{Program: "/opt/x$/sui-copilot", WorkDir: "/opt"},
		{Program: "/opt/x\"y", WorkDir: "/opt"},
		{Program: "/opt/x`y", WorkDir: "/opt"},
		{Program: "/opt/x y/sui-copilot", WorkDir: "/opt"},
		{Program: "/opt/x;y/sui-copilot", WorkDir: "/opt"},
		{Program: "/opt/x'y/sui-copilot", WorkDir: "/opt"},
		{Program: "opt/sui-copilot", WorkDir: "/opt"},
		{Program: "", WorkDir: "/opt"},
		{Program: "/opt/sui-copilot", WorkDir: ""},
		{Program: "/opt/sui-copilot", WorkDir: "/opt", Args: []string{"positional"}},
		{Program: "/opt/sui-copilot", WorkDir: "/opt", Args: []string{"--addr"}},
		{Program: "/opt/sui-copilot", WorkDir: "/opt", Args: []string{"--addr=1.2.3.4:80;reboot"}},
		{Program: "/opt/sui-copilot", WorkDir: "/opt", Args: []string{"--addr=1.2.3.4 80"}},
		{Program: "/opt/sui-copilot", WorkDir: "/opt", Args: []string{"--addr=$(reboot)"}},
	}
	for _, opts := range cases {
		if _, err := renderProcdScript(opts); err == nil {
			t.Errorf("expected rejection for %+v", opts)
		}
		if _, err := renderSystemdUnit(opts); err == nil {
			t.Errorf("systemd expected rejection for %+v", opts)
		}
	}
}

func TestRenderAcceptsTypicalValues(t *testing.T) {
	opts := Options{
		Program: "/opt/sui-copilot.v0.1.0/sui-copilot_linux-amd64",
		WorkDir: "/mnt/sda1/sui-copilot",
		Args:    []string{"--config=/mnt/sda1/sui-copilot/sui-copilot.toml", "--addr=192.168.1.1:8080"},
	}
	if _, err := renderProcdScript(opts); err != nil {
		t.Errorf("procd should accept typical values: %v", err)
	}
	if _, err := renderSystemdUnit(opts); err != nil {
		t.Errorf("systemd should accept typical values: %v", err)
	}
}

func TestProcdInstallSequence(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "sui-copilot")
	origPath := procdScriptPath
	procdScriptPath = scriptPath
	t.Cleanup(func() { procdScriptPath = origPath })

	runner := newFakeRunner()
	manager := &procdManager{run: runner}
	if err := manager.Install(testOptions()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !runner.has(scriptPath + " enable") {
		t.Errorf("missing enable, commands: %v", runner.commands)
	}
	if !runner.has(scriptPath + " restart") {
		t.Errorf("missing restart, commands: %v", runner.commands)
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("script not written: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("script mode = %v, want 0755", info.Mode().Perm())
	}

	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := manager.Uninstall(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !runner.has(scriptPath+" stop") || !runner.has(scriptPath+" disable") {
		t.Errorf("uninstall sequence wrong: %v", runner.commands)
	}
	if _, err := os.Stat(scriptPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("script should be removed, stat err = %v", err)
	}
}

func TestProcdEnableFailureLeavesScript(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "sui-copilot")
	origPath := procdScriptPath
	procdScriptPath = scriptPath
	t.Cleanup(func() { procdScriptPath = origPath })

	runner := newFakeRunner()
	runner.failOn[scriptPath+" enable"] = errors.New("exit 1")
	manager := &procdManager{run: runner}
	if err := manager.Install(testOptions()); err == nil {
		t.Fatal("install should fail when enable fails")
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("script should remain for debugging, stat err = %v", err)
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	unit, err := renderSystemdUnit(testOptions())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"WorkingDirectory=/opt/sui-copilot",
		"ExecStart=/opt/sui-copilot/sui-copilot-linux-amd64 --config=/opt/sui-copilot/sui-copilot.toml --addr=127.0.0.1:8080",
		"Restart=on-failure",
		"TimeoutStopSec=5s",
		"UMask=0077",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q\n%s", want, unit)
		}
	}
}

func TestRenderSystemdUnitRejectsWhitespace(t *testing.T) {
	if _, err := renderSystemdUnit(Options{Program: "/opt/a b/sui", WorkDir: "/opt"}); err == nil {
		t.Error("program path with space should be rejected")
	}
	if _, err := renderSystemdUnit(Options{Program: "/opt/sui", WorkDir: "/opt", Args: []string{"--addr=127.0.0.1:8080 extra"}}); err == nil {
		t.Error("arg with space should be rejected")
	}
}

func TestSystemdInstallSequence(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "sui-copilot.service")
	origPath := systemdUnitPath
	systemdUnitPath = unitPath
	t.Cleanup(func() { systemdUnitPath = origPath })

	runner := newFakeRunner()
	manager := &systemdManager{run: runner}
	if err := manager.Install(testOptions()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !runner.has("systemctl daemon-reload") {
		t.Errorf("missing daemon-reload: %v", runner.commands)
	}
	if !runner.has("systemctl enable sui-copilot.service") {
		t.Errorf("missing enable: %v", runner.commands)
	}
	if !runner.has("systemctl restart sui-copilot.service") {
		t.Errorf("missing restart (repeated install must apply the new definition): %v", runner.commands)
	}

	if err := manager.Uninstall(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !runner.has("systemctl disable --now sui-copilot.service") {
		t.Errorf("missing disable --now: %v", runner.commands)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit should be removed, stat err = %v", err)
	}
	if !runner.has("systemctl daemon-reload") {
		t.Errorf("missing uninstall daemon-reload: %v", runner.commands)
	}
}

func TestInstalledProgramPath(t *testing.T) {
	opts := testOptions()

	t.Run("procd", func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "sui-copilot")
		origPath := procdScriptPath
		procdScriptPath = scriptPath
		t.Cleanup(func() { procdScriptPath = origPath })

		manager := &procdManager{run: newFakeRunner()}
		if _, err := manager.InstalledProgramPath(); !errors.Is(err, ErrNotInstalled) {
			t.Errorf("want ErrNotInstalled, got %v", err)
		}
		script, err := renderProcdScript(opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := manager.InstalledProgramPath()
		if err != nil || got != opts.Program {
			t.Errorf("installedProgramPath = %q, %v; want %q", got, err, opts.Program)
		}
	})

	t.Run("systemd", func(t *testing.T) {
		dir := t.TempDir()
		unitPath := filepath.Join(dir, "sui-copilot.service")
		origPath := systemdUnitPath
		systemdUnitPath = unitPath
		t.Cleanup(func() { systemdUnitPath = origPath })

		manager := &systemdManager{run: newFakeRunner()}
		if _, err := manager.InstalledProgramPath(); !errors.Is(err, ErrNotInstalled) {
			t.Errorf("want ErrNotInstalled, got %v", err)
		}
		unit, err := renderSystemdUnit(opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := manager.InstalledProgramPath()
		if err != nil || got != opts.Program {
			t.Errorf("installedProgramPath = %q, %v; want %q", got, err, opts.Program)
		}
	})
}
