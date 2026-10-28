package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"sui-copilot/internal/config"
	"sui-copilot/internal/httpapi"
	"sui-copilot/internal/store"
)

const defaultConfigName = "sui-copilot.toml"

// version is set at build time with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Printf("sui-copilot %s\n", version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		if err := runAuth(os.Args[2:]); err != nil {
			logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
			logger.Error("sui auth", "error", err)
			os.Exit(1)
		}
		return
	}

	configPath := configPathFromArgs(os.Args[1:], envOr("SUI_COPILOT_CONFIG", defaultConfigPath()))
	if absolutePath, err := filepath.Abs(configPath); err == nil {
		configPath = absolutePath
	}
	runtimeConfig, err := config.Load(configPath)
	if errors.Is(err, os.ErrNotExist) {
		runtimeConfig = config.Default()
		config.ApplyEnv(&runtimeConfig)
	} else if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("load config", "path", configPath, "error", err)
		os.Exit(1)
	}

	addr := flag.String("addr", runtimeConfig.Server.ListenAddr, "HTTP listen address")
	dataDir := flag.String("data-dir", runtimeConfig.Server.DataDir, "data directory")
	debug := flag.Bool("debug", false, "enable debug-only submit preview")
	flag.String("config", configPath, "TOML configuration file")
	flag.Parse()
	if !filepath.IsAbs(*dataDir) {
		*dataDir = filepath.Join(filepath.Dir(configPath), *dataDir)
	}
	configureSessionPaths(&runtimeConfig, *dataDir, filepath.Dir(configPath))

	logLevel, err := parseLogLevel(runtimeConfig.Logging.Level)
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("invalid log level", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	db, err := store.Open(*dataDir + "/sui-copilot.db")
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	server := httpapi.NewServerWithConfigPathAndDebug(db, logger, runtimeConfig, configPath, *debug)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, *addr); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func configureSessionPaths(cfg *config.Config, dataDir, configDir string) {
	if cfg.Sui.SessionFile == "" {
		cfg.Sui.SessionFile = filepath.Join(dataDir, "sui-session.bin")
	} else if !filepath.IsAbs(cfg.Sui.SessionFile) {
		cfg.Sui.SessionFile = filepath.Join(configDir, cfg.Sui.SessionFile)
	}
	if cfg.Sui.SessionKeyFile == "" {
		cfg.Sui.SessionKeyFile = filepath.Join(dataDir, "sui-session.key")
	} else if !filepath.IsAbs(cfg.Sui.SessionKeyFile) {
		cfg.Sui.SessionKeyFile = filepath.Join(configDir, cfg.Sui.SessionKeyFile)
	}
}

func defaultConfigPath() string {
	executable, err := os.Executable()
	if err != nil {
		return defaultConfigName
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return filepath.Join(filepath.Dir(executable), defaultConfigName)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func parseLogLevel(value string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(value))); err != nil {
		return 0, fmt.Errorf("%q (want debug, info, warn, or error)", value)
	}
	return level, nil
}

func configPathFromArgs(args []string, fallback string) string {
	for index, arg := range args {
		if (arg == "--config" || arg == "-config") && index+1 < len(args) {
			return args[index+1]
		}
		if strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "-config=") {
			return strings.SplitN(arg, "=", 2)[1]
		}
	}
	return fallback
}
