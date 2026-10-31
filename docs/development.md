# 开发与发布

## 环境

- Go 1.27
- Node.js（自带 npm）
- [Task](https://taskfile.dev/)（go-task）

`task setup` 会下载 Go 模块并安装前端依赖。

## 常用命令

- `task check`：`gofmt` 和 `go vet`
- `task test`：全量 Go 测试
- `task web:build`：构建并校验 Vue 前端
- `task web:embed`：把校验过的前端产物同步到 Go 的嵌入目录
- `task build`：构建当前 `GOOS/GOARCH` 的 `bin/sui-copilot-<os>-<arch>`
- `task build:linux`：构建 Linux amd64/arm64
- `task build:release`：构建全部发布平台
- `task build:target TARGET=windows/amd64`：只构建指定发布平台
- `task clean`：清理构建产物

前端依赖统一使用 npm：`task setup` 或 `task web:install` 安装依赖，前端构建使用 `npm --prefix web run build`。

## 构建流程

`task build` 会先构建并校验 Vue 前端，再把构建产物复制到 Go 的嵌入目录，最后生成二进制。前端依赖或构建产物缺失时，任务会直接失败，不会生成缺少静态资源的二进制。

开发时使用：

```sh
task dev --watch
```

默认读取 `bin/sui-copilot.toml`，相对 `data_dir` 按该配置文件目录解析。使用其他配置文件时设置 `SUI_COPILOT_CONFIG`。

## 下载与平台

[GitHub Releases](https://github.com/nov12/sui-copilot/releases) 提供正式版本。下载文件名为 `sui-copilot-<os>-<arch>`，Windows 以 `.exe` 结尾。所有二进制都包含网页资源，并以 `CGO_ENABLED=0` 构建，无需设备另行安装 SQLite、Go、Node.js 或 glibc。

| 系统 | 发布架构 |
| --- | --- |
| Linux | `386`、`amd64`、`armv5`、`armv6`、`armv7`、`arm64`、`loong64`、`ppc64le`、`riscv64`、`s390x` |
| Windows | `386`、`amd64`、`arm64` |
| macOS（文件名为 `darwin`） | `amd64`、`arm64` |
| FreeBSD | `amd64`、`arm64` |

共 17 个目标，清单统一维护在 [`scripts/release-targets.json`](../scripts/release-targets.json)。日常 Build 与正式 Release 使用同一构建流程；Build 的每个平台产物在 Actions 运行页面保留 14 天，正式 Release 提供持续可访问的下载入口。

`386` 是 32 位 x86，`amd64` 是 64 位 x86，适用于 Intel 和 AMD 处理器。`arm64` 是 64 位 ARM；`armv5`、`armv6`、`armv7` 分别使用 Go 的 `GOARM=5/6/7`，其中 v5 使用软件浮点，v6/v7 默认要求对应的硬件浮点支持。Go 运行时对操作系统和 CPU 的最低要求仍然适用。

路由器可运行与其**系统及用户空间架构**匹配的 Linux/FreeBSD 二进制，并非所有型号都能安装。需要足够的空闲内存、可写存储、网络访问和系统 CA 证书，下载前应核对固件架构。当前 `modernc.org/sqlite v1.52.0` 的[官方支持列表](https://pkg.go.dev/modernc.org/sqlite@v1.52.0#hdr-Supported_platforms_and_architectures)不包括 MIPS/MIPSEL，因此不提供这些平台的文件；支持它们需要另行评估数据库实现。

以 Linux amd64 为例，下载对应二进制、`sui-copilot.toml.sample` 和 `SHA256SUMS` 到同一目录后：

```sh
grep -E '  (sui-copilot-linux-amd64|sui-copilot\.toml\.sample)$' SHA256SUMS | sha256sum -c -
chmod +x sui-copilot-linux-amd64
cp sui-copilot.toml.sample sui-copilot.toml
./sui-copilot-linux-amd64 auth login --config ./sui-copilot.toml
./sui-copilot-linux-amd64 --config ./sui-copilot.toml
```

Windows 下载对应 `.exe` 和配置示例后，在 PowerShell 中运行，例如：

```powershell
Copy-Item sui-copilot.toml.sample sui-copilot.toml
.\sui-copilot-windows-amd64.exe auth login --config .\sui-copilot.toml
.\sui-copilot-windows-amd64.exe --config .\sui-copilot.toml
```

## 版本与发布

版本号来自 git tag，格式为 `vX.Y.Z`。构建时通过 `git describe --tags --always --dirty` 写入二进制：

```sh
./bin/sui-copilot-linux-amd64 --version
```

未打 tag 的本地构建会显示提交哈希或 `dev`；工作区有未提交改动时会带 `-dirty` 后缀。

发布流程：

```sh
git tag v0.1.0
git push origin v0.1.0
```

推送 `v*` tag 会触发 [Release workflow](../.github/workflows/release.yml)，复用 [Build workflow](../.github/workflows/build.yml) 完成测试、一次前端构建和全部平台的并行构建。所有构建成功后才创建 GitHub Release，附带 17 个二进制、配置示例、`SHA256SUMS`。发布说明由 GitHub 按本仓库的 conventional commits 提交类型自动分组生成（配置见 [.github/release.yml](../.github/release.yml)：`feat` → 新功能、`fix` → 修复、`docs` → 文档、`refactor` → 重构、`build`/`ci` → 构建），无需维护 CHANGELOG 文件。任一目标失败都会阻止发布。

普通分支推送只产生 Actions 构建产物，不会创建 Release；创建并推送版本标签是正式发布动作，需要仓库所有者批准。上述 `v0.1.0` 仅为命令示例。
