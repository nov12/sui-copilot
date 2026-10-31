package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"sui-copilot/internal/selfupdate"
	"sui-copilot/internal/service"
)

const updateUsage = `usage: sui-copilot update [flags]

从 GitHub Releases 下载并校验新版本，替换当前二进制；已注册系统服务且程序路径一致时自动重启。
升级在改动磁盘前先记录待重启状态；重启未完成时，下次运行 update 优先补上重启（无需网络）。

flags:
  --check            只检查是否有新版本（只读，不写本地文件），不下载不替换
  --version v0.2.0   升级（或回退）到指定版本；默认只在最新 Release 更新时升级
  --repo owner/name  覆盖 GitHub 仓库（默认 ` + selfupdate.DefaultRepo + `）
`

// runUpdate implements `sui-copilot update`.
func runUpdate(args []string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	checkOnly := flags.Bool("check", false, "only check for a newer release")
	wantVersion := flags.String("version", "", "upgrade (or downgrade) to a specific release tag")
	repo := flags.String("repo", selfupdate.DefaultRepo, "GitHub repository (owner/name)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "未知参数 %q\n\n", flags.Arg(0))
		fmt.Fprint(os.Stderr, updateUsage)
		return fmt.Errorf("unknown argument %q", flags.Arg(0))
	}
	if *checkOnly && *wantVersion != "" {
		return errors.New("--check 不能与 --version 同时使用")
	}

	executable, err := executablePath()
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 2 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// --check never touches the local installation, so it needs neither the
	// update lock nor write access to the binary directory.
	if *checkOnly {
		if selfupdate.RestartPending(executable) {
			fmt.Println("提示：上次升级后服务重启未完成，运行 sui-copilot update 补上。")
		}
		release, err := selfupdate.FetchRelease(ctx, httpClient, *repo, "")
		if err != nil {
			return fmt.Errorf("查询 Release 失败: %w", err)
		}
		fmt.Printf("当前版本：%s\n最新版本：%s\n", currentVersion(), release.TagName)
		switch selfupdate.CompareBaseVersions(currentVersion(), release.TagName) {
		case 0:
			fmt.Println("已是最新版本。")
		case -1:
			fmt.Println("有新版本可用：运行 sui-copilot update 升级。")
		case 1:
			fmt.Println("当前版本比最新 Release 更新（可能是未发布的构建）。")
		}
		return nil
	}

	// One lock covers the whole run — pending-restart recovery, version
	// decision, replacement, restart, marker cleanup — so concurrent update
	// processes cannot interleave those steps.
	lock, err := selfupdate.TryInstallLock(executable)
	if err != nil {
		return fmt.Errorf("create update lock: %w", err)
	}
	if lock == nil {
		return errors.New("另一个 update 进程正在进行，请稍后重试")
	}
	defer lock.Close()

	// Finish a previous run's restart first; this needs no network and must
	// not depend on the remote version.
	if selfupdate.RestartPending(executable) {
		fmt.Println("检测到上次升级后服务重启未完成，先补上。")
		if err := updateRestartService(executable); err != nil {
			return err
		}
	}

	release, err := selfupdate.FetchRelease(ctx, httpClient, *repo, *wantVersion)
	if err != nil {
		return fmt.Errorf("查询 Release 失败: %w", err)
	}
	comparison := selfupdate.CompareBaseVersions(currentVersion(), release.TagName)
	fmt.Printf("当前版本：%s\n目标版本：%s\n", currentVersion(), release.TagName)
	// Default runs only ever move forward: a locally newer build must not be
	// replaced by an older release. Explicit --version is the caller's
	// decision, including downgrades.
	if comparison >= 0 && *wantVersion == "" {
		fmt.Println("当前版本不落后于最新 Release，无需升级。")
		return nil
	}

	// The marker is written before anything on disk changes, so an interrupt
	// at any point leaves a state the next run can finish: the recovery path
	// either makes the replaced binary live, or restarts the still-old binary
	// harmlessly before updating again. A failed marker write aborts before
	// any change; a failed replacement clears the marker because nothing was
	// replaced.
	if err := selfupdate.MarkRestartPending(executable); err != nil {
		return fmt.Errorf("记录待重启状态失败，升级已取消: %w", err)
	}
	if err := performUpdate(ctx, httpClient, release, executable); err != nil {
		_ = selfupdate.ClearRestartPending(executable)
		return err
	}

	fmt.Printf("已替换为 %s。\n", release.TagName)
	return updateRestartService(executable)
}

// performUpdate downloads, verifies, extracts, and swaps in the new binary.
// All temporary files live next to the executable so the final renames stay on
// one filesystem and remain atomic. The download lives in its own
// subdirectory because a raw-binary asset extracts under the same name it was
// downloaded as.
func performUpdate(ctx context.Context, httpClient *http.Client, release *selfupdate.Release, executable string) error {
	dir := filepath.Dir(executable)
	assetName := selfupdate.AssetNameForExecutable(executable)
	tempDir, err := os.MkdirTemp(dir, ".sui-copilot-update-")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	downloadDir := filepath.Join(tempDir, "download")
	if err := os.MkdirAll(downloadDir, 0o700); err != nil {
		return err
	}

	archivePath := filepath.Join(downloadDir, assetName)
	if _, err := selfupdate.DownloadAndVerify(ctx, httpClient, release, assetName, archivePath); err != nil {
		return err
	}
	newBinary, err := selfupdate.ExtractBinary(archivePath, tempDir)
	if err != nil {
		return fmt.Errorf("解压新版本失败: %w", err)
	}
	if err := selfupdate.ReplaceBinary(executable, newBinary); err != nil {
		return fmt.Errorf("替换二进制失败: %w", err)
	}
	return nil
}

// restartServiceAfterUpdate restarts the service only when it is registered
// for the binary that was just replaced. Non-root cannot restart the service
// and the caller may not be able to read the unit; both cases report the
// manual step without failing the update, and the pending-restart marker lets
// a later root run finish the restart. Read errors other than "not installed"
// must fail loudly: the binary is new while the service state is unknown.
func restartServiceAfterUpdate(executable string) error {
	if requireRootCheck() {
		fmt.Println("已替换二进制；请手动执行 sudo sui-copilot service restart 重启服务。")
		return nil
	}
	manager := detectService()
	if manager == nil {
		fmt.Println("当前系统不支持服务管理，请手动重启进程。")
		_ = selfupdate.ClearRestartPending(executable)
		return nil
	}
	installed, err := service.InstalledProgramPath(manager)
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		fmt.Println("未注册系统服务，无需重启。")
		_ = selfupdate.ClearRestartPending(executable)
		return nil
	case err != nil:
		return fmt.Errorf("二进制已替换，但无法读取服务定义: %w（请手动执行 sudo sui-copilot service restart）", err)
	}
	if installed != executable {
		fmt.Printf("服务指向 %q，与本次替换的 %q 不同，未自动重启。\n", installed, executable)
		_ = selfupdate.ClearRestartPending(executable)
		return nil
	}
	if err := manager.Restart(); err != nil {
		return fmt.Errorf("二进制已替换，但重启服务失败: %w（新版本在服务重启后生效）", err)
	}
	_ = selfupdate.ClearRestartPending(executable)
	fmt.Println("服务已重启，新版本已生效。")
	return nil
}

// executablePath resolves the running executable with symlinks evaluated.
func executablePath() (string, error) {
	if path := currentExecutablePath; path != "" {
		return path, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return executable, nil
}

// currentExecutablePath overrides executablePath in tests; production leaves
// it empty.
var currentExecutablePath = ""
