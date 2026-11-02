package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"sui-copilot/internal/config"
	"sui-copilot/internal/sui"
)

const authUsage = `usage: sui-copilot auth login [flags]

commands:
  login              登录随手记并加密保存会话

login flags:
  --config path      TOML 配置文件（默认 SUI_COPILOT_CONFIG 或可执行文件同目录的 sui-copilot.toml）
  --data-dir path    覆盖数据目录（相对路径按配置文件目录解析）
  -h, --help         显示帮助并退出
`

func runAuth(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, authUsage)
		return errors.New("auth: missing command")
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, authUsage)
		return nil
	case "login":
	default:
		fmt.Fprint(os.Stderr, authUsage)
		return fmt.Errorf("auth: unknown command %q", args[0])
	}

	flags := flag.NewFlagSet("auth login", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(flags.Output(), authUsage) }
	configPathFlag := flags.String("config", envOr("SUI_COPILOT_CONFIG", defaultConfigPath()), "TOML configuration file")
	dataDirFlag := flags.String("data-dir", "", "data directory override")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("auth login: unknown argument %q", flags.Arg(0))
	}

	configPath, err := filepath.Abs(*configPathFlag)
	if err != nil {
		return err
	}
	runtimeConfig, err := loadConfigForAuth(configPath)
	if err != nil {
		return err
	}
	dataDir := runtimeConfig.Server.DataDir
	if *dataDirFlag != "" {
		dataDir = *dataDirFlag
	}
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(filepath.Dir(configPath), dataDir)
	}
	configureSessionPaths(&runtimeConfig, dataDir, filepath.Dir(configPath))
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(runtimeConfig.Sui.SessionFile), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(runtimeConfig.Sui.SessionKeyFile), 0o700); err != nil {
		return err
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "随手记账号: ")
	email, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("随手记账号不能为空")
	}
	fmt.Fprint(os.Stderr, "随手记密码: ")
	passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return fmt.Errorf("读取密码失败: %w", err)
	}
	password := string(passwordBytes)
	for index := range passwordBytes {
		passwordBytes[index] = 0
	}
	defer func() { password = "" }()

	metadata, err := sui.Login(context.Background(), runtimeConfig.Sui.BaseURL, email, password, runtimeConfig.Sui.SessionFile, runtimeConfig.Sui.SessionKeyFile)
	if err != nil {
		return fmt.Errorf("登录随手记失败: %w", err)
	}
	if runtimeConfig.Sui.BookName != "" {
		if err := sui.ValidateBook(metadata, runtimeConfig.Sui.BookName); err != nil {
			return fmt.Errorf("登录成功但目标账本校验失败: %w", err)
		}
	}
	fmt.Fprintf(os.Stdout, "登录成功：当前账本 %q，共 %d 个账本；会话已加密保存。\n", metadata.CurrentBook.Name, len(metadata.Books))
	return nil
}

func loadConfigForAuth(configPath string) (config.Config, error) {
	runtimeConfig, err := config.Load(configPath)
	if errors.Is(err, os.ErrNotExist) {
		runtimeConfig = config.Default()
		config.ApplyEnv(&runtimeConfig)
		return runtimeConfig, nil
	}
	if err != nil {
		return config.Config{}, err
	}
	return runtimeConfig, nil
}
