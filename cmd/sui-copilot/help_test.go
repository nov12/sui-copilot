package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sui-copilot/internal/buildinfo"
	"sui-copilot/internal/service"
)

func TestCLIProcess(t *testing.T) {
	if os.Getenv("SUI_COPILOT_CLI_TEST") != "1" {
		return
	}
	detectService = func() service.Manager {
		panic("help must not detect or mutate system services")
	}
	for index, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"sui-copilot"}, os.Args[index+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing CLI arguments")
}

func runCLIProcess(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	command.Env = append(os.Environ(), "SUI_COPILOT_CLI_TEST=1")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("CLI did not exit: args=%q output=%s", args, output)
	}
	return string(output), err
}

func TestCLIHelp(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "broken.toml")
	if err := os.WriteFile(configPath, []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUI_COPILOT_CONFIG", configPath)
	rootTokens := []string{"sui-copilot " + buildinfo.Version, "auth login", "service", "install", "uninstall", "start", "stop", "restart", "status", "update", "--addr", "--config", "--data-dir", "--debug", "-v, --version", "-h, --help", "SUI_COPILOT_CONFIG", "GITHUB_TOKEN"}
	authTokens := []string{"auth login", "--config", "--data-dir", "--help"}
	serviceTokens := []string{"install", "uninstall", "start", "stop", "restart", "status", "--config", "--addr", "--data-dir", "--help"}
	updateTokens := []string{"--check", "--version", "--repo", "--help"}
	type helpCase struct {
		args   []string
		tokens []string
	}
	cases := []helpCase{
		{[]string{"help"}, rootTokens},
		{[]string{"help", "auth", "login"}, authTokens},
		{[]string{"help", "service", "install"}, serviceTokens},
		{[]string{"help", "update"}, updateTokens},
		{[]string{"--config", configPath, "--data-dir", filepath.Join(dir, "data"), "-h"}, rootTokens},
	}
	for _, helpFlag := range []string{"-h", "--help"} {
		cases = append(cases,
			helpCase{[]string{helpFlag}, rootTokens},
			helpCase{[]string{"auth", helpFlag}, authTokens},
			helpCase{[]string{"auth", "login", helpFlag}, authTokens},
			helpCase{[]string{"service", helpFlag}, serviceTokens},
			helpCase{[]string{"update", helpFlag}, updateTokens},
		)
		for _, command := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
			cases = append(cases, helpCase{[]string{"service", command, helpFlag}, serviceTokens})
		}
	}
	for _, test := range cases {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			output, err := runCLIProcess(t, test.args...)
			if err != nil {
				t.Fatalf("help exited unsuccessfully: %v\n%s", err, output)
			}
			for _, token := range test.tokens {
				if !strings.Contains(output, token) {
					t.Errorf("help is missing %q:\n%s", token, output)
				}
			}
			for _, line := range strings.Split(output, "\n") {
				fields := strings.Fields(line)
				if len(fields) > 0 && strings.HasPrefix(fields[0], "-") && !strings.HasPrefix(fields[0], "--") && fields[0] != "-h," && fields[0] != "-v," {
					t.Errorf("help uses a nonstandard flag: %s", line)
				}
			}
		})
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "broken.toml" {
		t.Fatalf("help created files: %v", files)
	}
}

func TestCLIVersionIgnoresInvalidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "broken.toml")
	if err := os.WriteFile(configPath, []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUI_COPILOT_CONFIG", configPath)
	for _, args := range [][]string{{"--version"}, {"-v"}, {"--config", configPath, "--version"}, {"--config=" + configPath, "-v"}} {
		output, err := runCLIProcess(t, args...)
		if err != nil || output != "sui-copilot "+buildinfo.Version+"\n" {
			t.Fatalf("version args=%q: err=%v output=%q", args, err, output)
		}
	}
}

func TestCLIInvalidArgumentsFail(t *testing.T) {
	for _, args := range [][]string{
		{"unknown-command"},
		{"--unknown-flag"},
		{"-version"},
		{"--v"},
		{"-help"},
		{"--h"},
		{"-config", "unused.toml", "-v"},
		{"-addr=127.0.0.1:8080", "-v"},
		{"-data-dir", "unused", "-v"},
		{"-debug", "-v"},
		{"auth", "login", "unexpected"},
		{"auth", "login", "-config", "unused.toml"},
		{"auth", "login", "-data-dir", "unused"},
		{"service", "install", "--unknown-flag"},
		{"service", "install", "-config", "unused.toml"},
		{"service", "install", "-addr", "127.0.0.1:8080"},
		{"service", "install", "-data-dir", "unused"},
		{"update", "unexpected"},
		{"update", "-check"},
		{"update", "-version", "v0.2.3"},
		{"update", "-repo", "owner/name"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			output, err := runCLIProcess(t, args...)
			if err == nil || !strings.Contains(output, "sui-copilot:") {
				t.Fatalf("invalid arguments must fail with a diagnostic: err=%v output=%q", err, output)
			}
		})
	}
}
