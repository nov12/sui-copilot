# 运行、配置与接口

本文件说明服务运行时的行为、配置和 HTTP 接口。快速上手见 [README](../README.md)。

## 启动恢复

服务启动时会把上一次进程退出时遗留的 `processing` 记录标记为解析失败，把遗留的 `committing` 记录转为 `uncertain` 待核对，并保留草稿和状态事件；不会把未完成的上游请求当成成功或自动重放。

## 日期与自动提交

模型返回的日期只要是合法的 `YYYY-MM-DD HH:mm`，小时和分钟都有，即使原文只有日期或模糊时段，也具备自动提交条件；不能用 `00:00` 冒充无法确定的时间。

## 随手记写入

随手记写入要求实际配置并验证会话、目标账本、分类和账户。请在网页登录随手记，服务会加密保存 CookieJar。确认操作会调用随手记写入接口；明确拒绝会保留为 `rejected`，超时或无法解释的写入结果会保留为 `uncertain`，调用方不得自动重试。写入响应兼容标准 JSON 和随手记实际返回的无引号字段名对象（例如 `{id:{id:12345},price:0.01}`）；收到有效正数流水 ID 后直接标记为 `committed`，无需手动核对。成功说明保留在状态事件中，当前错误字段清空；启动迁移也会清理历史已入账记录中残留的错误字段。

## 超时与失败处理

后台单笔处理总时限为 10 分钟；每次随手记请求另有 30 秒时限（包括该请求触发的 SSO 恢复），调用方更短的时限仍然生效，避免单个请求长时间占用队列。HTTP 响应写超时为 60 秒，按最慢的合法操作（同步账簿分类约 9 个限频请求）设定，仍是硬上限，卡住的连接不会无限挂起。请求超时或调用方断开后，提交结果使用独立的 5 秒时限保存到本地；写入结果不明确时保留为待核对，后续队列继续处理，不会重发记账请求。该本地保存仍可能因数据库或磁盘故障失败，服务会记录错误；遗留的提交状态将在下次启动时转为待核对。

## 配置

默认读取可执行文件同目录下的 `sui-copilot.toml`；相对 `data_dir` 按配置文件目录解析。也可以用 `--config`、`--addr` 和 `--data-dir` 覆盖。示例配置见 [sui-copilot.toml.sample](../sui-copilot.toml.sample)。

通过 `task dev` 使用其他配置文件时设置 `SUI_COPILOT_CONFIG`；`task dev` 默认使用 `bin/sui-copilot.toml`。

在「配置 → 接口接入」填写外部客户端可访问的地址，例如 `https://book.example.com`。服务把它保存到 TOML 的 `[server].public_url`，网页的 HTTP API 命令和 MCP 配置都使用这个地址。留空时使用当前网页地址。地址可带端口和代理路径前缀。使用前缀时，代理需将相应请求转发至服务根路径。

日志等级在配置文件中设置：

```toml
[logging]
level = "info" # debug / info / warn / error
```

默认使用 `info`。每次 HTTP 接口访问只在 `debug` 等级下记录；服务启动、后台维护完成等运行状态使用 `info`，错误使用 `error`。也可以用 `SUI_COPILOT_LOG_LEVEL` 环境变量覆盖配置文件中的等级。这里的 `logging.level = "debug"` 只控制日志详细程度，与命令行 `--debug` 开启提交预览是两项独立设置。

## 登录与会话

先启动服务，再在网页点击「登录随手记」或在「配置」里重新登录。服务使用随手记的“保持登录状态”流程，校验登录后可访问账簿和记账页，然后加密保存 CookieJar 并立即启用新会话。密码仅用于本次请求。升级后可继续使用原数据目录中的会话文件，不需要重新登录。

访问账簿首页时，程序会尝试用已保存的长期 Cookie 恢复过期的短会话。长期凭据失效时，网页会提示重新登录。如果随手记要求账号验证，请先在随手记官网完成验证。已有失败或待核对的记账记录需要按原状态处理，登录成功不会触发重试。正常提交与核对都先校验当前账簿和本地分类快照对应的账簿 ID。

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

校验不通过或缺少校验文件时升级终止，不会替换二进制；没有跳过校验的开关。替换通过同目录临时文件一次性 rename 完成，任意时刻可执行文件路径上都是完整的旧版或新版，中断不会留下无法启动的状态，也不保留旧版备份（运行中的进程持有自己的 inode）。默认只在当前版本落后于目标时升级；当前版本更新（如未发布的构建）或相同时不替换，回退到旧版本需显式指定 `--version`。版本号仅比较 `vX.Y.Z` 三元组：`git describe` 后缀（如 `v0.1.0-5-gabcdef`、`-dirty`）视为与对应发布同版本；源码自建的 `dev` 版本视为可升级到任意发布。实际更新与手动 `service restart` 共用二进制所在目录上的非阻塞文件锁串行化：更新进行中时重启会被拒绝，锁不创建任何文件，随进程退出自动释放，`--check` 只读查询不需要目录写权限。升级在改动磁盘前先写入待重启标记 `<二进制名>.restart-pending`：升级中断或重启失败时，下次运行 `update` 会先自动补上重启（无需网络），补上或确认无需重启后才继续后续升级；标记写入失败则升级取消，磁盘不做任何改动；手动 `service restart` 成功同样会清除标记。升级完成后只有确认已注册服务的程序路径与本次替换的二进制一致才自动重启，重启失败会返回非零退出码；非 root 或未注册服务时提示手动重启但不报错。ARM32 资产按可执行文件名解析 armv5/6/7 变体，无法从文件名判断时使用 armv5 软浮点构建（在全部 ARM32 CPU 上可运行）。发布资产优先使用对应平台压缩包（Unix 为 `.tar.gz`，Windows 为 `.zip`），也兼容旧版裸二进制；先校验下载资产，再仅解包对应程序，不覆盖配置示例或现有配置。压缩包缺少对应程序、程序条目为链接、超过大小限制或内容不是可执行文件时升级失败。Windows 不允许替换正在运行的程序映像，升级运行中的进程会失败，请先退出进程再执行 `update`；macOS、FreeBSD 暂不支持 `service` 子命令，`update` 可替换二进制但需手动重启进程。

旧版 `v0.2.0` 只支持裸二进制，首次升级到压缩包发布版本需手动下载、校验并解压，停止服务后替换原安装路径的程序，再启动服务；保留现有配置和数据。之后可恢复使用 `update`。

## Debug 提交预览

需要查看或执行 debug 提交预览时，直接运行带 `--debug` 的服务：

```sh
./bin/sui-copilot-<os>-<arch> --config ./bin/sui-copilot.toml --debug
```

`--debug` 才会开放提交预览和手动提交接口。每份预览绑定生成时的会话和账簿。重新登录、重启服务或切换到其他账簿后，请重新生成预览。

## HTTP 接口

- `GET /api/v1/health` 返回 `{"status":"ok","version":"<构建版本>"}`，网页使用该版本号，响应不缓存。
- 服务启动后立即在后台检查一次 GitHub 最新 Release，此后每 6 小时检查一次；关闭网页不影响检查，停止服务时取消正在进行的检查。每次检查最长 15 秒，版本比较规则与 CLI 相同。仅提示新版，不自动下载或安装。自动检查失败只记日志，保留上次成功结果，等待下次定时检查；也可点击版本号立即重试。
- `GET /api/v1/updates/status` 只读取最近一次成功检查的内存缓存，不访问 GitHub，返回 `currentVersion`、`latestVersion`、`updateAvailable`、`releaseUrl` 与 `checkedAt`。`checkedAt` 为成功检查的 UTC 时间，尚未成功检查时为空。网页打开、回到前台时读取，前台每分钟再读取一次；发现新版时显示提示条，无新版时保持安静。
- `GET /api/v1/updates` 由网页点击版本号触发，每次实际查询 GitHub，并更新与后台共用的缓存；响应不缓存。失败返回错误，不会误报为最新版，也不会清除已有新版提示。手动检查无新版时短暂显示淡色提示。CLI 的 `update --check` 仍可独立检查。
- 更新状态还包含 `canAutoUpdate`、`updating`、`updatingVersion` 和 `updateError`，分别表示是否支持网页更新、是否正在更新、此次更新固定的目标版本及最近更新错误。`updatingVersion` 不随后台发现另一个新版本而改变。
- `POST /api/v1/updates` 接受 JSON `{"version":"vX.Y.Z"}`，只允许更新到已检查且有更新可用的版本；接受后返回 `202`，重复操作或版本过期返回 `409`，不支持网页更新返回 `501`。请求必须是 JSON，跨站 Origin 被拒绝，不接受自定义仓库等额外字段。反向代理需要透传原始 `Host`（如 Nginx 的 `proxy_set_header Host $http_host`），接口不采信任意 `X-Forwarded-Host`。现有单用户访问隔离要求仍然适用，不应把写接口直接暴露到公网。
- 网页更新仅适用于 root 运行、已注册 systemd / OpenWrt 服务且服务定义指向当前二进制的安装，使用与 CLI 相同的安装锁、下载、SHA-256 校验、原子替换和待重启恢复机制。其他运行方式禁用网页更新按钮并提示 CLI / 手动重启。网页确认目标版本已运行后刷新；失败或等待超时会明确提示，不代表旧进程已经切换到新版本。
- 网页更新由系统服务管理器完成自重启，不能在进程内部运行阻塞的外部 CLI 重启流程。待重启标记包含目标版本，目标进程启动后才确认清除；重启请求失败或更新中断时保留标记，并明确提示先使用 `sui-copilot update` 恢复，不覆盖未完成更新。CLI 补上待完成重启不需要网络，之后若继续查询新版仍需网络。
- `POST /api/v1/ingest` 接收文本并立即保存为 `queued` 待解析记录，后台使用当前选择的模型服务按最近同步的当前账簿资料解析；字段明确时自动提交到随手记，只有不确定字段才进入人工确认。后台记账流程按提交顺序进入单工 FIFO 队列，同一时刻只处理一笔，避免连续记账时并行调用模型和随手记接口；人工确认提交也共享随手记写入闸门。同一输入类型和输入文本已有记录时返回 `409 duplicate_entry`，不会再次调用 AI。
- 服务启动时会将残留的 `processing` 记录转为 `rejected`，错误代码为 `processing_interrupted`，提示重新解析；残留的 `committing` 记录转为 `uncertain`，错误代码为 `sui_commit_interrupted`，先核对流水，不自动重试写入。
- `GET /api/v1/entries`、`GET /api/v1/entries/{id}` 查询记录。
- `PATCH /api/v1/entries/{id}` 按版本保存草稿。
- `POST /api/v1/entries/{id}/reparse` 立即把记录放回 `queued` 待解析队列；后台使用当前账簿资料和解析补充提示重新生成草稿，字段明确时自动提交，字段不确定时进入待人工确认。`uncertain` 待核对记录不能重新解析：其随手记写入结果未知，可能已入账，重新解析会走自动提交而造成重复记账；请先核对流水或使用人工处理接口。
- `POST /api/v1/entries/{id}/interrupt` 可以中断 `processing` 中的记录，错误代码为 `processing_cancelled`。文本记录可重新解析，结构化记录可在网页检查草稿后提交。
- `POST /api/v1/entries/reconcile-uncertain` 批量核对不确定记录；页面“刷新并核对”会调用它，服务也会每 5 分钟自动调用一次。页面对待核对记录每 15 秒读取本地状态，并在后台核对完成后更新详情；轮询不会额外请求随手记。
- `POST /api/v1/entries/{id}/confirm` 确认处理；`POST /api/v1/entries/{id}/resolve` 人工处理 `uncertain` 或 `rejected` 记录：核对随手记流水后指定 `result` 为 `committed`（确认已入账）或 `rejected`（确认未入账），可附 `note` 说明依据。`DELETE /api/v1/entries/{id}` 删除记录。
- `POST /api/v1/processing/pause` 与 `POST /api/v1/processing/resume` 暂停和恢复后台解析队列。暂停只影响后台领取新任务：已在解析中的记录会照常完成，人工确认提交不受影响；队列里的待解析记录保留，恢复后按原顺序继续。
- `GET/PATCH /api/v1/settings` 管理网页配置，包括对外访问地址 `server_public_url`、模型服务商、模型服务地址、模型和解析补充 prompt。随手记地址只在 TOML 中配置。模型服务 API Key 只允许写入，GET 返回“已配置”状态。网页保存的配置直接原子写入启动时使用的 TOML 文件，文件权限为 owner-only。
- `GET /api/v1/sui/session` 检查当前会话。网页打开时检查一次，已保存会话使用账簿首页验证并尝试恢复短会话。成功时返回 `authenticated`，长期会话失效时返回 `401 sui_session_expired`。
- `POST /api/v1/sui/session` 接受同源 JSON 请求 `{"account":"…","password":"…"}`。成功后返回当前账簿、账簿列表以及现有分类快照是否仍可使用。登录响应不缓存。反向代理需保留浏览器使用的原始 `Host`。
- `GET /api/v1/llm/models`（`/api/v1/models` 和旧的 `/api/v1/openrouter/models` 兼容别名）读取当前模型服务缓存的模型目录；`POST /api/v1/llm/models/sync` 同步当前服务商的 `/models` 列表。自定义端点不支持 `/models` 时仍可手填模型 ID。
- `GET /api/v1/sui/books` 读取 SQLite 中缓存的随手记账簿列表；`POST /api/v1/sui/books/sync` 才访问随手记并更新缓存。选择目标账簿会同时保存到 SQLite，网页中点击“同步当前账簿分类”后才由 `POST /api/v1/sui/sync` 同步完整分类资料和更新时间。分类按一级/二级层级展示，记账分类只能选择二级分类。

## 外部程序提交结构化账目

已有账目字段的程序可以直接提交结构化账目。服务复用网页的后台队列、账簿校验和随手记写入流程，整个过程无需配置或调用模型服务。账目进入队列后，到达 `committed` 才表示已写入随手记。

### HTTP API

1. 用 `GET /api/v1/metadata` 取得已同步的当前账簿 `bookId`、二级分类和账户。请先在网页登录随手记、选择目标账簿并同步分类。
2. 向 `POST /api/v1/entries` 发送 `Content-Type: application/json` 的请求。每笔账目使用唯一的 `clientRequestId`。下面是支出示例，`bookId`、分类和账户需取自你的账簿。

   ```json
   {
     "clientRequestId": "3c8448bc-98d5-4a3d-a662-63bb7c4d5f3c",
     "bookId": "123456",
     "draft": {
       "type": "expense",
       "date": "2026-11-19 12:30",
       "amount": "35.00",
       "categoryName": "餐饮 / 午餐",
       "accountName": "银行卡",
       "memo": "午饭"
     }
   }
   ```

3. 首次接收返回 `202 Accepted`，例如 `{"entryId":42,"state":"queued","existing":false}`。使用 `GET /api/v1/entries/42` 查询 `entry.state`。

`type` 支持 `expense`（支出）与 `income`（收入）。`date` 使用北京时间，格式为 `YYYY-MM-DD HH:mm`。`amount` 是正数金额字符串，最多两位小数。`categoryName` 使用对应收入或支出的二级分类路径，`accountName` 使用账簿中的账户名称。可选字段有 `memo`、`storeName`、`projectName`、`memberName` 和 `needsConfirmation`。填写商家、项目或成员时，请使用当前账簿的名称。

网络超时后以**原请求 ID 和原参数**重试。记录仍在本地时，重试返回 `200 OK`、原 `entryId` 与 `existing: true`。同一个 ID 用于不同账簿或内容会返回 `409 idempotency_conflict`。已清理或手动删除的记录不再参与此检查。

| 状态 | 含义 |
| --- | --- |
| `queued`、`processing`、`committing` | 已接收，等待校验或写入，可继续查询。 |
| `committed` | 随手记已入账。 |
| `needs_confirmation` | 字段未通过账簿校验，或提交时指定了 `needsConfirmation: true`。可在网页记录详情处理。 |
| `rejected` | 写入失败，`errorCode` 和 `errorMessage` 给出原因。 |
| `uncertain` | 写入结果未知，先核对随手记流水，避免重记。 |
| `pending_submission` | 草稿已经校验，等待提交。Debug 模式使用网页预览。 |

如果目标账簿已切换，需要切回记录所属账簿并同步后再提交。服务在校验阶段退出时，重启后记录会重新进入队列。已经进入写入阶段的记录会恢复为 `uncertain`。

### MCP

在支持 **Streamable HTTP** 的 MCP 客户端中添加服务器。网页「配置 → 接口接入 → MCP」提供可复制的地址、Codex 配置、Claude Code 命令和工具参数。本机默认地址为 `http://127.0.0.1:8080/mcp`。

| 工具 | 用法 |
| --- | --- |
| `get_book_metadata` | 读取当前账簿 ID、北京时间、分类路径、账户、商家、项目和成员名称。 |
| `submit_entry` | 接收与 `POST /api/v1/entries` 相同的参数，返回记录 ID 和状态。 |
| `get_entry_status` | 传入 `{"entryId":42}`，查询这条结构化记录的处理状态及错误原因。 |

HTTP API 和 MCP 使用相同的请求 ID 去重规则。跨设备接入沿用[安全边界](#安全边界)中的 HTTPS 反向代理与认证。代理到回环地址时，`/mcp` 的上游 `Host` 使用回环地址（例如 `127.0.0.1:8080`），以通过 MCP 的主机名检查。网页和其他接口继续使用浏览器的原始 `Host`。

## 后台维护

每天凌晨 2:00（Asia/Shanghai）执行：删除本地保存时间超过 7 天的 `committed` 记录及其状态事件，然后执行 SQLite `VACUUM` 回收空间。维护不会删除 `uncertain`、`needs_confirmation` 或其他待人工处理记录。

## 模型服务

模型服务配置位于 TOML 的 `[llm]` 段：`provider` 可选 `openai`、`openrouter`、`custom`。第一版只支持 OpenAI-compatible Chat Completions，不支持 Responses API 或旧的 `/completions` 文本接口。OpenAI 和 OpenRouter 使用内置 Base URL；自定义服务需要填写 Base URL，并提供 `{base_url}/chat/completions`，且最好提供 `{base_url}/models`。三种服务商都必须支持当前使用的 `response_format.type = "json_schema"`。系统不会在服务商之间自动 fallback。旧的 `[openrouter]` 配置仍可读取，网页下次保存时迁移到 `[llm]`。

解析请求省略 `temperature`，使用所选模型的默认采样设置，兼容只接受默认温度的模型。

## 安全边界

本项目按个人单用户场景设计。登录接口校验请求来源，部署侧仍需控制访问权限。服务默认监听 `127.0.0.1:8080`，请勿直接暴露到公网。从其他设备访问时，请使用带认证的 HTTPS 反向代理，并保留原始 `Host`。真实随手记写入前需要受控验收。
