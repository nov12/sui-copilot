package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sui-copilot/internal/selfupdate"
	"sui-copilot/internal/service"
)

const serviceUsage = `usage: sui-copilot service <command> [flags]

commands:
  install     注册为系统服务并启动（需要 root）
  uninstall   停止并移除系统服务（需要 root）
  start       启动服务
  stop        停止服务
  restart     重启服务
  status      查看服务状态

install flags:
  --config path    配置文件路径（默认与可执行文件同目录）
  --addr host:port 覆盖监听地址（仅显式指定时写入服务定义）
  --data-dir path  覆盖数据目录（仅显式指定时写入服务定义）

支持 OpenWrt（procd）和 systemd；其他系统暂不支持。
`

// runService implements `sui-copilot service <command>`.
func runService(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, serviceUsage)
		return errors.New("service: missing command")
	}
	command, rest := args[0], args[1:]

	configPath, explicit, err := resolveConfigPathForSubcommand(rest)
	if err != nil {
		return err
	}
	if command == "install" {
		return serviceInstall(configPath, explicit)
	}

	manager := detectService()
	if manager == nil {
		return errors.New("当前系统不受支持：需要 OpenWrt（procd）或 systemd")
	}
	switch command {
	case "uninstall":
		return requireRoot(func() error { return manager.Uninstall() })
	case "start":
		return requireRoot(manager.Start)
	case "stop":
		return requireRoot(manager.Stop)
	case "restart":
		return requireRoot(func() error {
			executable, err := executablePath()
			if err != nil {
				return err
			}
			// Share the update lock: while an update is running, a restart
			// would bring back the old binary and clearing the pending
			// marker would break the update's recovery path.
			lock, err := selfupdate.TryInstallLock(executable)
			if err != nil {
				return fmt.Errorf("获取更新锁失败: %w", err)
			}
			if lock == nil {
				return errors.New("另一个 update 进程正在进行，请稍后再重启")
			}
			defer lock.Close()
			if err := manager.Restart(); err != nil {
				return err
			}
			// A successful restart also completes a pending update's
			// restart, so drop the marker to avoid a duplicate restart later.
			_ = selfupdate.ClearRestartPending(executable)
			return nil
		})
	case "status":
		output, err := manager.Status()
		if output != "" {
			fmt.Print(output)
		}
		return err
	case "-h", "--help", "help":
		fmt.Print(serviceUsage)
		return nil
	default:
		fmt.Fprintf(os.Stderr, "service: unknown command %q\n\n", command)
		fmt.Fprint(os.Stderr, serviceUsage)
		return fmt.Errorf("service: unknown command %q", command)
	}
}

// serviceInstall registers the service with the detected init backend.
func serviceInstall(configPath string, explicit *explicitFlags) error {
	manager := detectService()
	if manager == nil {
		return errors.New("当前系统不受支持：需要 OpenWrt（procd）或 systemd")
	}
	return requireRoot(func() error {
		options, err := buildServiceOptions(configPath, explicit)
		if err != nil {
			return err
		}
		if err := manager.Install(options); err != nil {
			return err
		}
		fmt.Printf("服务已注册（%s）并启动。\n", manager.Name())
		fmt.Printf("  程序：%s\n  目录：%s\n", options.Program, options.WorkDir)
		if len(options.Args) > 0 {
			fmt.Printf("  参数：%s\n", strings.Join(options.Args, " "))
		}
		fmt.Println("日志：OpenWrt 使用 logread，systemd 使用 journalctl -u sui-copilot。")
		return nil
	})
}

// buildServiceOptions assembles the persisted service definition. Only flags
// the user passed explicitly are persisted; the rest come from the config file
// at runtime. An explicitly given config must be readable: without it the
// service would silently start with default settings instead of the user's.
func buildServiceOptions(configPath string, explicit *explicitFlags) (service.Options, error) {
	if explicit.configPath != "" {
		file, err := os.Open(configPath)
		if err != nil {
			return service.Options{}, fmt.Errorf("无法读取配置文件 %s: %w（服务将使用默认配置启动，已取消安装）", configPath, err)
		}
		file.Close()
	}
	executable, err := os.Executable()
	if err != nil {
		return service.Options{}, fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	workDir := filepath.Dir(configPath)
	if explicit.dataDir != "" {
		workDir = filepath.Dir(explicit.dataDir)
		if !filepath.IsAbs(workDir) {
			return service.Options{}, fmt.Errorf("--data-dir 必须是绝对路径：%q", explicit.dataDir)
		}
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return service.Options{}, fmt.Errorf("create work directory: %w", err)
	}
	options := service.Options{
		Program: executable,
		WorkDir: workDir,
		Args:    []string{"--config=" + configPath},
	}
	if explicit.addr != "" {
		options.Args = append(options.Args, "--addr="+explicit.addr)
	}
	if explicit.dataDir != "" {
		options.Args = append(options.Args, "--data-dir="+explicit.dataDir)
	}
	return options, nil
}

// explicitFlags records which flags the user passed on the command line.
type explicitFlags struct {
	configPath string
	addr       string
	dataDir    string
}

// resolveConfigPathForSubcommand parses the flags accepted by service
// management subcommands and reports which ones were set explicitly.
func resolveConfigPathForSubcommand(args []string) (string, *explicitFlags, error) {
	flags := flag.NewFlagSet("service", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	explicit := &explicitFlags{}
	flags.StringVar(&explicit.configPath, "config", "", "configuration file")
	flags.StringVar(&explicit.addr, "addr", "", "listen address")
	flags.StringVar(&explicit.dataDir, "data-dir", "", "data directory")
	if err := flags.Parse(args); err != nil {
		return "", nil, err
	}
	if flags.NArg() > 0 {
		return "", nil, fmt.Errorf("未知参数：%q", flags.Arg(0))
	}
	configPath := explicit.configPath
	if configPath == "" {
		configPath = os.Getenv("SUI_COPILOT_CONFIG")
	}
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return "", nil, err
	}
	explicit.configPath = configPath
	return absolute, explicit, nil
}

// requireRoot rejects commands that need root with a clear message. Service
// management is unsupported on platforms without the POSIX root concept, and
// Detect() already returns nil there.
func requireRoot(action func() error) error {
	if requireRootCheck() {
		return errors.New("需要 root 权限：请用 sudo 或直接以 root 执行")
	}
	return action()
}

// Hooks for command-level tests. Production builds use the real
// implementations; tests substitute fakes without any framework.
var (
	detectService        = service.Detect
	requireRootCheck     = func() bool { return os.Geteuid() != 0 }
	updateRestartService = restartServiceAfterUpdate
	currentVersion       = func() string { return version }
)
