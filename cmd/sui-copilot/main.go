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

	"sui-copilot/internal/buildinfo"
	"sui-copilot/internal/config"
	"sui-copilot/internal/httpapi"
	"sui-copilot/internal/store"
)

const defaultConfigName = "sui-copilot.toml"

const rootUsage = `usage: sui-copilot [flags]
       sui-copilot <command> [flags]

不带子命令时启动 HTTP 服务和网页。

commands:
  auth login          登录随手记并加密保存会话
  service <command>   管理系统服务：install、uninstall、start、stop、restart、status
  update              从 GitHub Releases 检查或升级程序
  help [command]      查看帮助（也可使用 <command> -h 或 --help）

flags:
  --addr host:port    HTTP 监听地址（覆盖配置文件）
  --config path      TOML 配置文件（默认 SUI_COPILOT_CONFIG 或可执行文件同目录的 sui-copilot.toml）
  --data-dir path    数据目录（相对路径按配置文件目录解析）
  --debug            开启仅用于调试的提交预览
  -v, --version      显示程序版本并退出
  -h, --help         显示帮助并退出

environment:
  SUI_COPILOT_CONFIG  默认配置文件路径；--config 优先，否则使用可执行文件同目录的 sui-copilot.toml
  GITHUB_TOKEN        update 查询 GitHub Releases 时使用的可选令牌
`

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "sui-copilot: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "auth":
			return runAuth(args[1:])
		case "service":
			return runService(args[1:])
		case "update":
			return runUpdate(args[1:])
		case "help":
			return run(append(args[1:], "--help"))
		}
	}

	defaults := config.Default()
	flags := flag.NewFlagSet("sui-copilot", flag.ContinueOnError)
	addr := flags.String("addr", defaults.Server.ListenAddr, "HTTP listen address (overrides configuration)")
	dataDir := flags.String("data-dir", defaults.Server.DataDir, "data directory (relative to configuration file)")
	debug := flags.Bool("debug", false, "enable debug-only submit preview")
	configPathFlag := flags.String("config", envOr("SUI_COPILOT_CONFIG", defaultConfigPath()), "TOML configuration file")
	showVersion := flags.Bool("version", false, "print program version and exit")
	flags.BoolVar(showVersion, "v", false, "print program version and exit")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "sui-copilot %s\n\n%s", buildinfo.Version, rootUsage)
	}
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		flags.Usage()
		return fmt.Errorf("unknown command or argument %q", flags.Arg(0))
	}
	if *showVersion {
		fmt.Printf("sui-copilot %s\n", buildinfo.Version)
		return nil
	}
	configPath, err := filepath.Abs(*configPathFlag)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	runtimeConfig, err := config.Load(configPath)
	if errors.Is(err, os.ErrNotExist) {
		runtimeConfig = config.Default()
		config.ApplyEnv(&runtimeConfig)
	} else if err != nil {
		return fmt.Errorf("load config %s: %w", configPath, err)
	}

	flags.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "addr":
			runtimeConfig.Server.ListenAddr = *addr
		case "data-dir":
			runtimeConfig.Server.DataDir = *dataDir
		}
	})
	*addr = runtimeConfig.Server.ListenAddr
	*dataDir = runtimeConfig.Server.DataDir
	if !filepath.IsAbs(*dataDir) {
		*dataDir = filepath.Join(filepath.Dir(configPath), *dataDir)
	}
	configureSessionPaths(&runtimeConfig, *dataDir, filepath.Dir(configPath))

	logLevel, err := parseLogLevel(runtimeConfig.Logging.Level)
	if err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	db, err := store.Open(*dataDir + "/sui-copilot.db")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	server := httpapi.NewServerWithConfigPathAndDebug(db, logger, runtimeConfig, configPath, *debug)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, *addr); err != nil {
		return fmt.Errorf("server stopped: %w", err)
	}
	return nil
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
