# 开发与发布

## 环境

- Go 1.27
- Node.js（自带 npm）
- [Task](https://taskfile.dev/)（go-task）

发布构建还需要 `tar`（Unix 平台压缩包）和 `zip`（Windows 压缩包）；脚本会在编译前检查所选目标需要的工具。

`task setup` 会下载 Go 模块并安装前端依赖。

## 常用命令

- `task check`：`gofmt` 和 `go vet`
- `task test`：全量 Go 测试
- `task web:build`：构建并校验 Vue 前端
- `task web:embed`：把校验过的前端产物同步到 Go 的嵌入目录
- `task build`：构建当前 `GOOS/GOARCH` 的 `bin/sui-copilot-<os>-<arch>`
- `task build:linux`：构建 Linux amd64/arm64
- `task build:release`：构建全部发布平台，在 `bin/release/` 生成压缩包及 `SHA256SUMS`
- `task build:target TARGET=windows/amd64`：只构建指定平台的压缩包及 `SHA256SUMS`
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

[GitHub Releases](https://github.com/nov12/sui-copilot/releases) 提供正式版本。Linux、macOS、FreeBSD 下载文件名为 `sui-copilot-<os>-<arch>.tar.gz`，Windows 为 `sui-copilot-windows-<arch>.zip`。每个压缩包包含对应平台的二进制和 `sui-copilot.toml.sample`，不需要单独下载配置示例。所有二进制都包含网页资源，并以 `CGO_ENABLED=0` 构建，无需设备另行安装 SQLite、Go、Node.js 或 glibc。

| 系统 | 发布架构 |
| --- | --- |
| Linux | `386`、`amd64`、`armv5`、`armv6`、`armv7`、`arm64`、`loong64`、`ppc64le`、`riscv64`、`s390x` |
| Windows | `386`、`amd64`、`arm64` |
| macOS（文件名为 `darwin`） | `amd64`、`arm64` |
| FreeBSD | `amd64`、`arm64` |

共 17 个目标，清单统一维护在 [`scripts/release-targets.json`](../scripts/release-targets.json)。本地 Task 和 CI 共用 [`scripts/build-release.mjs`](../scripts/build-release.mjs) 完成编译、打包和校验文件生成，每次运行重新生成 `bin/release/`。仅推送版本 tag 触发云端发布构建；普通 push、PR、手动操作不触发构建。CI 汇总各平台压缩包，生成覆盖全部平台的校验清单后发布；不会上传裸二进制或独立配置示例。

`386` 是 32 位 x86，`amd64` 是 64 位 x86，适用于 Intel 和 AMD 处理器。`arm64` 是 64 位 ARM；`armv5`、`armv6`、`armv7` 分别使用 Go 的 `GOARM=5/6/7`，其中 v5 使用软件浮点，v6/v7 默认要求对应的硬件浮点支持。Go 运行时对操作系统和 CPU 的最低要求仍然适用。

路由器可运行与其**系统及用户空间架构**匹配的 Linux/FreeBSD 二进制，并非所有型号都能安装。需要足够的空闲内存、可写存储、网络访问和系统 CA 证书，下载前应核对固件架构。当前 `modernc.org/sqlite v1.52.0` 的[官方支持列表](https://pkg.go.dev/modernc.org/sqlite@v1.52.0#hdr-Supported_platforms_and_architectures)不包括 MIPS/MIPSEL，因此不提供这些平台的文件；支持它们需要另行评估数据库实现。

以 Linux amd64 为例，下载对应 `.tar.gz` 和 `SHA256SUMS` 到同一目录后，先校验压缩包，再解压运行：

```sh
grep '  sui-copilot-linux-amd64.tar.gz$' SHA256SUMS | sha256sum -c -
tar -xzf sui-copilot-linux-amd64.tar.gz
cp sui-copilot.toml.sample sui-copilot.toml
./sui-copilot-linux-amd64 auth login --config ./sui-copilot.toml
./sui-copilot-linux-amd64 --config ./sui-copilot.toml
```

Windows 下载对应 `.zip` 和 `SHA256SUMS` 后，先比较压缩包的 SHA-256（必须与清单中同名文件的值一致），再解压运行。例如：

```powershell
Get-FileHash .\sui-copilot-windows-amd64.zip -Algorithm SHA256
Get-Content .\SHA256SUMS | Select-String '  sui-copilot-windows-amd64.zip$'
Expand-Archive .\sui-copilot-windows-amd64.zip -DestinationPath .\sui-copilot
Set-Location .\sui-copilot
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

发布说明直接从 git 历史生成，无需手写 CHANGELOG 或维护额外的版本章节。提交沿用仓库的中文 Conventional Commit 惯例，在正文中写清功能、修复和使用限制，发布时会完整保留。发布命令示例：

```sh
git tag v0.1.0
git push origin v0.1.0
```

推送 `vX.Y.Z` tag 会触发 [Release workflow](../.github/workflows/release.yml)，复用仅接受发布调用的 [Build workflow](../.github/workflows/build.yml)，完成 Go 测试、一次前端构建和全部平台的并行编译及打包。所有目标成功后才创建 GitHub Release，附件仅包含 17 个平台压缩包和 `SHA256SUMS`。配置示例随包提供，不单独上传；压缩包中的 Unix 程序保留执行权限。发布说明收录当前 tag 的祖先中最近的版本 tag（`v[0-9]*`）之后至当前 tag 的全部非合并提交标题与正文，并附完整比较链接；首次发布收录当前 tag 可达的全部非合并提交。任一目标失败都会阻止发布。

普通分支推送、PR 和手动操作不运行该构建流程，也不产生 Actions 构建产物。创建并推送版本标签是正式发布动作，需要仓库所有者批准。上述 `v0.1.0` 仅为命令示例，不应重新推送已有标签。

### 从裸二进制版本升级

`v0.2.0` 的更新命令只支持裸二进制，不能自动安装新格式压缩包。首次跨越此变更时，下载并校验对应压缩包，解压到临时目录，停止现有服务，将解压出的程序替换到原安装路径后启动服务；保留原配置与数据，不要用示例覆盖现有配置。之后可继续使用新版本的 `update`。新版本也支持旧版裸二进制资产，便于通过 `--version` 显式回退；回退到旧版后，再跨越格式变更仍需手动安装。本次变更不修改已有 Release 的附件或 tag。
