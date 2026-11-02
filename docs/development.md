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

[GitHub Releases](https://github.com/nov12/sui-copilot/releases) 提供正式版本。Linux、macOS、FreeBSD 下载文件名为 `sui-copilot-<os>-<arch>.tar.gz`，Windows 为 `sui-copilot-windows-<arch>.zip`。每个压缩包只包含对应平台的二进制；配置示例 `sui-copilot.toml.sample` 保留在仓库，需要时从所下载版本的 tag 页面获取。所有二进制都包含网页资源，并以 `CGO_ENABLED=0` 构建，无需设备另行安装 SQLite、Go、Node.js 或 glibc。

| 系统 | 发布架构 |
| --- | --- |
| Linux | `386`、`amd64`、`armv5`、`armv6`、`armv7`、`arm64`、`loong64`、`ppc64le`、`riscv64`、`s390x` |
| Windows | `386`、`amd64`、`arm64` |
| macOS（文件名为 `darwin`） | `amd64`、`arm64` |
| FreeBSD | `amd64`、`arm64` |

共 17 个目标，清单统一维护在 [`scripts/release-targets.json`](../scripts/release-targets.json)。本地 Task 和 CI 共用 [`scripts/build-release.mjs`](../scripts/build-release.mjs) 完成编译、打包和校验文件生成，每次运行重新生成 `bin/release/`。仅推送版本 tag 触发云端发布构建；普通 push、PR、手动操作不触发构建。CI 汇总各平台压缩包，生成覆盖全部平台的校验清单后发布；不会上传裸二进制或独立配置示例。

`386` 是 32 位 x86，`amd64` 是 64 位 x86，适用于 Intel 和 AMD 处理器。`arm64` 是 64 位 ARM；`armv5`、`armv6`、`armv7` 分别使用 Go 的 `GOARM=5/6/7`，其中 v5 使用软件浮点，v6/v7 默认要求对应的硬件浮点支持。Go 运行时对操作系统和 CPU 的最低要求仍然适用。

路由器可运行与其**系统及用户空间架构**匹配的 Linux/FreeBSD 二进制，并非所有型号都能安装。需要足够的空闲内存、可写存储、网络访问和系统 CA 证书，下载前应核对固件架构。当前 `modernc.org/sqlite v1.52.0` 的[官方支持列表](https://pkg.go.dev/modernc.org/sqlite@v1.52.0#hdr-Supported_platforms_and_architectures)不包括 MIPS/MIPSEL，因此不提供这些平台的文件；支持它们需要另行评估数据库实现。

以 Linux amd64 为例，下载对应 `.tar.gz` 和 `SHA256SUMS` 到同一目录后，先校验压缩包，再解压运行。首次安装需要配置示例时，从对应 tag 的仓库页面另行下载 `sui-copilot.toml.sample` 到同一目录；下方复制配置的命令仅用于首次安装，升级时保留现有配置：

```sh
grep '  sui-copilot-linux-amd64.tar.gz$' SHA256SUMS | sha256sum -c -
tar -xzf sui-copilot-linux-amd64.tar.gz
cp sui-copilot.toml.sample sui-copilot.toml
./sui-copilot-linux-amd64 auth login --config ./sui-copilot.toml
./sui-copilot-linux-amd64 --config ./sui-copilot.toml
```

Windows 下载对应 `.zip` 和 `SHA256SUMS` 后，先比较压缩包的 SHA-256（必须与清单中同名文件的值一致），再解压运行。首次安装需要配置示例时，从对应 tag 的仓库页面另行下载 `sui-copilot.toml.sample`，放入解压后的 `sui-copilot` 目录；升级时跳过复制配置。例如：

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

版本号来自 git tag，格式为 `vX.Y.Z`。构建时通过 `git describe --tags --always --dirty` 写入二进制的 `sui-copilot/internal/buildinfo.Version`，CLI、网页和更新检查共用这一版本来源：

```sh
./bin/sui-copilot-linux-amd64 --version
```

未打 tag 的本地构建会显示提交哈希或 `dev`；工作区有未提交改动时会带 `-dirty` 后缀。

网页标题上方与 `SUI COPILOT` 同行显示正在运行的后端版本，通过 `GET /api/v1/health` 读取，不使用前端 `package.json` 的版本号。服务启动后检查一次新版，此后每 6 小时在后台检查；网页读取缓存，发现新版时显示版本号与发布说明入口。点击淡色版本文字可立即手动检查，检查中显示小转圈，没有新版时短暂显示淡色提示。直接使用 `go run` 或未注入版本号的 `go build` 时显示 `dev`。

发布说明来自仓库的 [CHANGELOG.md](../CHANGELOG.md)，不直接复制提交信息。平时把尚未发布的变化写入 `## [Unreleased]`；准备发布时，将本次条目整理到与 tag 完全一致的版本章节，例如 `## [vX.Y.Z] - YYYY-MM-DD`，再保留一个新的 `Unreleased` 章节。

每版先用一段话说明重点，再按需要使用 `### 新增`、`### 改进`、`### 修复`、`### 升级注意` 等分类，只保留有内容的分类。每项用列表写清用户可见的行为、适用条件和影响，命令及文件名使用行内代码；需要用户操作时提供步骤或代码块。不要粘贴开发过程、测试清单或整段提交正文；完整提交记录由发布页末尾的比较链接提供。

发布前运行 `node --test scripts/*.test.mjs` 和 `node scripts/release-notes.mjs vX.Y.Z`，检查提取结果。脚本只读取指定版本的章节，缺失或没有变更条目时失败，不会用 `Unreleased` 或提交正文代替。版本章节必须先随代码提交，再创建 tag。发布命令示例：

```sh
git tag v0.1.0
git push origin v0.1.0
```

推送 `vX.Y.Z` tag 会触发 [Release workflow](../.github/workflows/release.yml)，复用仅接受发布调用的 [Build workflow](../.github/workflows/build.yml)。流程先验证对应版本的发布说明，再完成 Go 测试、一次前端构建和全部平台的并行编译及打包。所有目标成功后才创建 GitHub Release，附件仅包含 17 个平台压缩包和 `SHA256SUMS`。压缩包内只有对应平台的程序，Unix 程序保留执行权限。发布正文使用 CHANGELOG 的对应版本条目，自动附加安装与校验说明、该 tag 的配置示例链接，以及当前 tag 与其祖先中最近的版本 tag（`v[0-9]*`）的完整比较链接；首次发布附提交历史链接。说明校验或任一目标失败都会阻止发布。

普通分支推送、PR 和手动操作不运行该构建流程，也不产生 Actions 构建产物。创建并推送版本标签是正式发布动作，需要仓库所有者批准。上述 `v0.1.0` 仅为命令示例，不应重新推送已有标签。

### 从裸二进制版本升级

`v0.2.0` 的更新命令只支持裸二进制，不能自动安装新格式压缩包。首次跨越此变更时，下载并校验对应压缩包，解压到临时目录，停止现有服务，将解压出的程序替换到原安装路径后启动服务；保留原配置与数据，不要用示例覆盖现有配置。之后可继续使用新版本的 `update`。新版本也支持旧版裸二进制资产，便于通过 `--version` 显式回退；回退到旧版后，再跨越格式变更仍需手动安装。本次变更不修改已有 Release 的附件或 tag。
