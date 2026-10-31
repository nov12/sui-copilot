# 运行、配置与接口

本文件说明服务运行时的行为、配置和 HTTP 接口。快速上手见 [README](../README.md)。

## 启动恢复

服务启动时会把上一次进程退出时遗留的 `processing` 记录标记为解析失败，把遗留的 `committing` 记录转为 `uncertain` 待核对，并保留草稿和状态事件；不会把未完成的上游请求当成成功或自动重放。

## 日期与自动提交

模型返回的日期只要是合法的 `YYYY-MM-DD HH:mm`，小时和分钟都有，即使原文只有日期或模糊时段，也具备自动提交条件；不能用 `00:00` 冒充无法确定的时间。

## 随手记写入

随手记写入要求实际配置并验证会话、目标账本、分类和账户。必须使用 `auth login` 建立并加密保存 CookieJar；不再支持手工 Cookie 配置。确认操作会调用随手记写入接口；明确拒绝会保留为 `rejected`，超时或无法解释的写入结果会保留为 `uncertain`，调用方不得自动重试。写入响应兼容标准 JSON 和随手记实际返回的无引号字段名对象（例如 `{id:{id:12345},price:0.01}`）；收到有效正数流水 ID 后直接标记为 `committed`，无需手动核对。成功说明保留在状态事件中，当前错误字段清空；启动迁移也会清理历史已入账记录中残留的错误字段。

## 超时与失败处理

后台单笔处理总时限为 10 分钟；每次随手记请求另有 30 秒时限（包括该请求触发的 SSO 恢复），调用方更短的时限仍然生效，避免单个请求长时间占用队列。HTTP 响应写超时为 60 秒，按最慢的合法操作（同步账簿分类约 9 个限频请求）设定，仍是硬上限，卡住的连接不会无限挂起。请求超时或调用方断开后，提交结果使用独立的 5 秒时限保存到本地；写入结果不明确时保留为待核对，后续队列继续处理，不会重发记账请求。该本地保存仍可能因数据库或磁盘故障失败，服务会记录错误；遗留的提交状态将在下次启动时转为待核对。

## 配置

默认读取可执行文件同目录下的 `sui-copilot.toml`；相对 `data_dir` 按配置文件目录解析。也可以用 `--config`、`--addr` 和 `--data-dir` 覆盖。示例配置见 [sui-copilot.toml.sample](../sui-copilot.toml.sample)。

通过 `task dev` 使用其他配置文件时设置 `SUI_COPILOT_CONFIG`；`task dev` 默认使用 `bin/sui-copilot.toml`。

日志等级在配置文件中设置：

```toml
[logging]
level = "info" # debug / info / warn / error
```

默认使用 `info`。每次 HTTP 接口访问只在 `debug` 等级下记录；服务启动、后台维护完成等运行状态使用 `info`，错误使用 `error`。也可以用 `SUI_COPILOT_LOG_LEVEL` 环境变量覆盖配置文件中的等级。这里的 `logging.level = "debug"` 只控制日志详细程度，与命令行 `--debug` 开启提交预览是两项独立设置。

## 登录与会话

`auth login` 会在终端中读取账号和密码，使用随手记网页当前的“保持登录状态”流程完成 SSO。密码不会保存；会话 CookieJar 会加密保存到配置对应的数据目录，文件权限为 owner-only。网页短会话过期时，程序会在读取账本首页时完成 SSO 自动提交表单，使用已保存的长期登录 Cookie 恢复会话并加密保存，无需重新输入密码。只有长期登录凭据也失效、被撤销或需要人工验证时，才需重新执行同一命令。不要把 Cookie 粘贴到聊天、模型服务或普通日志中。

建议先完成 `auth login` 再启动服务；服务进程会共享一份会话和请求限频状态。运行中手工重新登录后重启服务，让它加载新的会话文件。自动 SSO 恢复直接更新进程内会话，无需重启。恢复只接受登录站点到配置站点账本首页的 SSO 表单，不重放记账请求或切换账本请求；正常记账仍先校验目标账本，会话保存失败也会返回错误。

## 服务管理与升级

在 OpenWrt（procd）和 systemd 系统上，`service` 子命令把程序注册为系统服务，开机自启、异常退出自动拉起（对应 procd `respawn 3600 5 5`、systemd `Restart=on-failure`，停止时先发 TERM 并等待 5 秒）：

```sh
sudo ./sui-copilot-<os>-<arch> service install --config /opt/sui-copilot/sui-copilot.toml
sudo ./sui-copilot-<os>-<arch> service status
sudo ./sui-copilot-<os>-<arch> service stop    # start / stop / restart / status / uninstall
```

`install` 需要root，并要求程序、配置文件、数据目录使用绝对路径（默认即配置文件目录）；重复执行 `install` 会刷新服务定义并重启。仅显式传入的 `--addr` 和 `--data-dir` 会固化进服务定义，其余配置以 `--config` 指向的 TOML 文件为准。服务以 root 运行；OpenWrt 日志用 `logread | grep sui-copilot`，systemd 用 `journalctl -u sui-copilot`。程序和配置路径中的字符限制为字母、数字和 `._-/:=@[]`，包含空格或其他特殊字符的路径会被拒绝。

`update` 子命令从 GitHub Releases 下载新版本：查询最新 Release（或 `--version vX.Y.Z` 指定版本）→ 下载对应平台资产 → 按 `SHA256SUMS` 校验 → 原子替换当前二进制 → 已注册服务且指向同一二进制时自动重启：

```sh
sudo ./sui-copilot-<os>-<arch> update --check   # 只检查，不下载
sudo ./sui-copilot-<os>-<arch> update           # 升级到最新并重启服务
sudo ./sui-copilot-<os>-<arch> update --version v0.2.0
```

校验不通过或缺少校验文件时升级终止，不会替换二进制；没有跳过校验的开关。替换通过同目录临时文件一次性 rename 完成，任意时刻可执行文件路径上都是完整的旧版或新版，中断不会留下无法启动的状态，也不保留旧版备份（运行中的进程持有自己的 inode）。默认只在当前版本落后于目标时升级；当前版本更新（如未发布的构建）或相同时不替换，回退到旧版本需显式指定 `--version`。版本号仅比较 `vX.Y.Z` 三元组：`git describe` 后缀（如 `v0.1.0-5-gabcdef`、`-dirty`）视为与对应发布同版本；源码自建的 `dev` 版本视为可升级到任意发布。实际更新与手动 `service restart` 共用二进制所在目录上的非阻塞文件锁串行化：更新进行中时重启会被拒绝，锁不创建任何文件，随进程退出自动释放，`--check` 只读查询不需要目录写权限。升级在改动磁盘前先写入待重启标记 `<二进制名>.restart-pending`：升级中断或重启失败时，下次运行 `update` 会先自动补上重启（无需网络），补上或确认无需重启后才继续后续升级；标记写入失败则升级取消，磁盘不做任何改动；手动 `service restart` 成功同样会清除标记。升级完成后只有确认已注册服务的程序路径与本次替换的二进制一致才自动重启，重启失败会返回非零退出码；非 root 或未注册服务时提示手动重启但不报错。ARM32 资产按可执行文件名解析 armv5/6/7 变体，无法从文件名判断时使用 armv5 软浮点构建（在全部 ARM32 CPU 上可运行）。发布资产为裸二进制；校验通过但内容不是可执行文件时升级失败。API 限流时可设置 `GITHUB_TOKEN` 环境变量提高配额。Windows 不允许替换正在运行的程序映像，升级运行中的进程会失败，请先退出进程再执行 `update`；macOS、FreeBSD 暂不支持 `service` 子命令，`update` 可替换二进制但需手动重启进程。

## Debug 提交预览

需要查看或执行 debug 提交预览时，直接运行带 `--debug` 的服务：

```sh
./bin/sui-copilot-<os>-<arch> --config ./bin/sui-copilot.toml --debug
```

`--debug` 才会开放提交预览和手动提交接口；普通启动不会显示或接受这些接口。

## HTTP 接口

- `POST /api/v1/ingest` 接收文本并立即保存为 `queued` 待解析记录，后台使用当前选择的模型服务按最近同步的当前账簿资料解析；字段明确时自动提交到随手记，只有不确定字段才进入人工确认。后台记账流程按提交顺序进入单工 FIFO 队列，同一时刻只处理一笔，避免连续记账时并行调用模型和随手记接口；人工确认提交也共享随手记写入闸门。同一输入类型和输入文本已有记录时返回 `409 duplicate_entry`，不会再次调用 AI。
- 服务启动时会将残留的 `processing` 记录转为 `rejected`，错误代码为 `processing_interrupted`，提示重新解析；残留的 `committing` 记录转为 `uncertain`，错误代码为 `sui_commit_interrupted`，先核对流水，不自动重试写入。
- `GET /api/v1/entries`、`GET /api/v1/entries/{id}` 查询记录。
- `PATCH /api/v1/entries/{id}` 按版本保存草稿。
- `POST /api/v1/entries/{id}/reparse` 立即把记录放回 `queued` 待解析队列；后台使用当前账簿资料和解析补充提示重新生成草稿，字段明确时自动提交，字段不确定时进入待人工确认。`uncertain` 待核对记录不能重新解析：其随手记写入结果未知，可能已入账，重新解析会走自动提交而造成重复记账；请先核对流水或使用人工处理接口。
- `POST /api/v1/entries/{id}/interrupt` 只允许处理 `processing` 记录：先将其标记为可重新解析的失败记录，再取消当前模型请求，不需要重启服务；错误代码为 `processing_cancelled`。
- `POST /api/v1/entries/reconcile-uncertain` 批量核对不确定记录；页面“刷新并核对”会调用它，服务也会每 5 分钟自动调用一次。页面对待核对记录每 15 秒读取本地状态，并在后台核对完成后更新详情；轮询不会额外请求随手记。
- `POST /api/v1/entries/{id}/confirm` 确认处理；`POST /api/v1/entries/{id}/resolve` 人工处理 `uncertain` 或 `rejected` 记录：核对随手记流水后指定 `result` 为 `committed`（确认已入账）或 `rejected`（确认未入账），可附 `note` 说明依据。`DELETE /api/v1/entries/{id}` 删除记录。
- `POST /api/v1/processing/pause` 与 `POST /api/v1/processing/resume` 暂停和恢复后台解析队列。暂停只影响后台领取新任务：已在解析中的记录会照常完成，人工确认提交不受影响；队列里的待解析记录保留，恢复后按原顺序继续。
- `GET/PATCH /api/v1/settings` 管理网页配置，包括模型服务商、API Base URL、模型和解析补充 prompt。随手记地址只在 TOML 中配置，不通过网页设置接口暴露。模型服务 API Key 只允许写入，GET 只返回“已配置”状态，不返回原文；网页保存的配置直接原子写入启动时使用的 TOML 文件，文件权限为 owner-only。随手记会话只能通过 `auth login` 建立和更新。
- `GET /api/v1/llm/models`（`/api/v1/models` 和旧的 `/api/v1/openrouter/models` 兼容别名）读取当前模型服务缓存的模型目录；`POST /api/v1/llm/models/sync` 同步当前服务商的 `/models` 列表。自定义端点不支持 `/models` 时仍可手填模型 ID。
- `GET /api/v1/sui/books` 读取 SQLite 中缓存的随手记账簿列表；`POST /api/v1/sui/books/sync` 才访问随手记并更新缓存。选择目标账簿会同时保存到 SQLite，网页中点击“同步当前账簿分类”后才由 `POST /api/v1/sui/sync` 同步完整分类资料和更新时间。分类按一级/二级层级展示，记账分类只能选择二级分类。

## 后台维护

每天凌晨 2:00（Asia/Shanghai）执行：删除本地保存时间超过 7 天的 `committed` 记录及其状态事件，然后执行 SQLite `VACUUM` 回收空间。维护不会删除 `uncertain`、`needs_confirmation` 或其他待人工处理记录。

## 模型服务

模型服务配置位于 TOML 的 `[llm]` 段：`provider` 可选 `openai`、`openrouter`、`custom`。第一版只支持 OpenAI-compatible Chat Completions，不支持 Responses API 或旧的 `/completions` 文本接口。OpenAI 和 OpenRouter 使用内置 Base URL；自定义服务需要填写 Base URL，并提供 `{base_url}/chat/completions`，且最好提供 `{base_url}/models`。三种服务商都必须支持当前使用的 `response_format.type = "json_schema"`。系统不会在服务商之间自动 fallback。旧的 `[openrouter]` 配置仍可读取，网页下次保存时迁移到 `[llm]`。

## 安全边界

本项目是个人使用项目，仅供个人在本地或可信内网运行：API 还没有管理员登录和 CSRF 层，没有公网所需的安全加固，不应直接暴露到公网。服务默认监听 `127.0.0.1:8080`，如需从其他设备访问，应放在带认证的 HTTPS 反向代理后。真实随手记写入前，仍需要受控验收。
