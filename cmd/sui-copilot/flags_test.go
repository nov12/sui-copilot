package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		wantConfig  string
		wantDebug   bool
		wantVersion bool
		wantArgs    []string
		wantError   string
		wantHelp    bool
	}{
		{name: "short version", args: []string{"-v"}, wantVersion: true},
		{name: "long version", args: []string{"--version"}, wantVersion: true},
		{name: "long flags", args: []string{"--config", "app.toml", "--debug", "-v"}, wantConfig: "app.toml", wantDebug: true, wantVersion: true},
		{name: "equals values", args: []string{"--config=app.toml", "--debug=false", "--version=true"}, wantConfig: "app.toml", wantVersion: true},
		{name: "flag-like value", args: []string{"--config", "-version", "-v"}, wantConfig: "-version", wantVersion: true},
		{name: "help-like value", args: []string{"--config", "--help", "-v"}, wantConfig: "--help", wantVersion: true},
		{name: "end of flags", args: []string{"--", "-config"}, wantArgs: []string{"-config"}},
		{name: "positional argument", args: []string{"argument", "-config"}, wantArgs: []string{"argument", "-config"}},
		{name: "short help", args: []string{"-h"}, wantHelp: true},
		{name: "long help", args: []string{"--help"}, wantHelp: true},
		{name: "help exits first", args: []string{"--help", "-config"}, wantHelp: true},
		{name: "single dash long", args: []string{"-config=app.toml"}, wantError: "use --config"},
		{name: "double dash short", args: []string{"--v"}, wantError: "use -v"},
		{name: "missing value", args: []string{"--config"}, wantError: "flag needs an argument"},
		{name: "unknown flag", args: []string{"--unknown"}, wantError: "flag provided but not defined"},
		{name: "invalid boolean", args: []string{"--debug=invalid"}, wantError: "invalid boolean value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			configPath := flags.String("config", "", "configuration file")
			debug := flags.Bool("debug", false, "debug mode")
			version := flags.Bool("version", false, "version")
			flags.BoolVar(version, "v", false, "version")
			err := parseFlags(flags, test.args)
			if test.wantHelp {
				if !errors.Is(err, flag.ErrHelp) {
					t.Fatalf("expected help, got %v", err)
				}
				return
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expected error containing %q, got %v", test.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if *configPath != test.wantConfig || *debug != test.wantDebug || *version != test.wantVersion {
				t.Fatalf("unexpected flags: config=%q debug=%v version=%v", *configPath, *debug, *version)
			}
			if !slices.Equal(flags.Args(), test.wantArgs) {
				t.Fatalf("remaining arguments = %q, want %q", flags.Args(), test.wantArgs)
			}
		})
	}
}

func TestServiceInstallFlagAliases(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "app.toml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	for _, args := range [][]string{
		{"--config", configPath, "--addr", "127.0.0.1:8080", "--data-dir", dataDir},
		{"-c", configPath, "-a", "127.0.0.1:8080", "-d", dataDir},
		{"-c=" + configPath, "-a=127.0.0.1:8080", "-d=" + dataDir},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resolved, explicit, err := resolveConfigPathForSubcommand(args, true)
			if err != nil {
				t.Fatal(err)
			}
			if resolved != configPath || explicit.configPath != configPath || explicit.addr != "127.0.0.1:8080" || explicit.dataDir != dataDir {
				t.Fatalf("aliases lost explicit values: config=%q flags=%+v", resolved, explicit)
			}
			options, err := buildServiceOptions(resolved, explicit)
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := []string{"--config=" + configPath, "--addr=127.0.0.1:8080", "--data-dir=" + dataDir}
			if !slices.Equal(options.Args, wantArgs) {
				t.Fatalf("service must persist canonical long flags: got=%q want=%q", options.Args, wantArgs)
			}
		})
	}
}
