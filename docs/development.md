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
- `task build:release`：构建 Linux/macOS 的 amd64/arm64
- `task clean`：清理构建产物

前端依赖统一使用 npm：`task setup` 或 `task web:install` 安装依赖，前端构建使用 `npm --prefix web run build`。

## 构建流程

`task build` 会先构建并校验 Vue 前端，再把构建产物复制到 Go 的嵌入目录，最后生成二进制。前端依赖或构建产物缺失时，任务会直接失败，不会生成缺少静态资源的二进制。

开发时使用：

```sh
task dev --watch
```

默认读取 `bin/sui-copilot.toml`，相对 `data_dir` 按该配置文件目录解析。使用其他配置文件时设置 `SUI_COPILOT_CONFIG`。

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

推送 `v*` tag 会触发 [Release workflow](../.github/workflows/release.yml)，构建 Linux/macOS 的 amd64/arm64 二进制并创建 GitHub Release，发布说明由 GitHub 自动生成。
