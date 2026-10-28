# 随手记记账桥接器：接口指南

更新时间：2026-09-08
状态：接口勘察完成，轻量登录与会话持久化已实现，真实账号验收待执行

本文把两部分接口分开说明：

1. 随手记网页当前实际使用的接口，供适配器实现。
2. 本项目建议对 iOS 快捷指令提供的内部接口，供后续工程实现。

## 1. 重要边界

- 随手记目前探测到的是网页私有接口，不是公开、稳定的开发者 API。
- 元数据（账本、分类、商家、账户、项目、成员）主要嵌在 `tally/new.do` 的 HTML 中，没有确认到独立的 JSON 元数据接口。
- 当前勘察只执行了页面读取和流水查询，没有执行新增、修改或删除账单。
- 随手记的当前账本由会话状态决定。写入前必须验证当前账本；本地只配置账簿名称，由服务端解析 ID。
- cookie 是登录凭证，不得提交给任何模型服务商，不得写入源码、Git、普通日志或错误响应。
- 自动化访问存在会话风控风险：即使请求是只读的，也可能因为来源 IP、TLS/HTTP 指纹、会话并发、请求节奏或缺少浏览器状态而触发强制退出。
- User-Agent 伪装不能视为规避风控的办法；不能通过不断换 User-Agent、加快重试或并发请求来“试出”可用方式。

## 2. 配置建议

正式工程必须先执行 `sui-copilot auth login`，让 Go 客户端完成随手记当前的保持登录状态流程并加密保存 CookieJar。服务端只从加密会话文件读取登录状态，不接受手工 Cookie 配置：

```text
SUI_BASE_URL=https://www.sui.com
SUI_SESSION_FILE=<encrypted session file, optional>
SUI_SESSION_KEY_FILE=<owner-only session key, optional>
SUI_BOOK_NAME=<允许写入的账本名称>
SUI_COPILOT_LOG_LEVEL=info
OPENROUTER_API_KEY=<OpenRouter API key>
LLM_PROVIDER=openrouter # openai / openrouter / custom
LLM_BASE_URL=<custom provider base URL, only for custom>
LLM_API_KEY=<current model service API key>
LLM_MODEL=<current model ID>
```

日志等级也可以写入启动配置文件：

```toml
[logging]
level = "info" # debug / info / warn / error
```

默认等级为 `info`。HTTP 接口访问日志使用 `debug`，服务状态日志使用 `info`，错误日志使用 `error`。`SUI_COPILOT_LOG_LEVEL` 会覆盖 TOML 中的 `logging.level`。这里的日志 `debug` 与命令行 `--debug` 的提交预览开关相互独立。日志中不得写入 Cookie、密码或任何模型服务 API Key。

本次勘察时当前账本显示为“自己账本”。工程中应配置名称而不是无条件写死 ID；服务端每次检查会话时从账簿列表解析名称，找不到或同名时明确报错，并重新确认当前账本。

### 2.1 轻量登录流程

随手记当前登录页先请求 `/login.do?opt=vccode`，再按网页脚本计算密码摘要并请求 `/login.do`，成功后访问 `/auth.do`，解析并提交其可能返回的隐藏表单，最后进入 `www.sui.com/sso/redirect.do`。VCCode 和登录响应当前返回普通 JSON，同时兼容旧版 JSONP。桥接器应使用 `http.CookieJar` 接收登录和 SSO 跳转中的全部 `Set-Cookie`，而不是从浏览器手工复制单个 Cookie。

“保持登录状态”对应登录请求中的 `status=1`。密码只在一次交互式登录进程中使用，不写入 TOML、SQLite、日志或 API 响应；会话文件使用本地 owner-only 密钥加密保存。服务进程启动时加载一份会话并共享请求限频状态；重新执行登录后应重启服务加载新会话。

## 3. 随手记会话与页面接口

### 3.1 获取概览页

```http
GET https://www.sui.com/report_index.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

用途：确认 cookie 是否有效、读取当前用户的账本列表和当前账本。

成功时返回 HTML，页面通常包含：

- 账本列表：`li[data-bookId]`
- 当前账本：`ssjBookSwitch_param.currentBookId` 或带 `current` class 的账本项
- 当前用户页面状态

如果被重定向到 `login.sui.com`，应返回“会话失效”，不要继续尝试写入。

### 3.3 自动化访问风控说明

本项目勘察过程中曾出现“只读页面请求完成后，用户浏览器会话被强制退出”的情况。由于服务端没有返回风控原因，不能证明一定由某一个请求直接造成，但这说明使用网页私有接口和实时 cookie 存在实际运行风险。

工程约束：

1. 不要让浏览器和服务器同时高频使用同一个会话 cookie。
2. 元数据刷新采用手动或低频策略，一次只执行必要的请求，不做并发抓取。
3. 遇到登录重定向、`id == -1`、连续 401/403 或响应结构异常时，立即停止后续请求。
4. 会话失效后不自动登录、不自动注销、不自动更换 cookie 重试；由用户重新执行 `sui-copilot auth login`，再恢复服务访问。
5. 不把 User-Agent 当成安全措施，也不以伪装客户端特征为目标。优先使用官方导出/接口；若只能使用网页私有接口，应接受其不稳定性和账号风控风险。
6. 服务器、浏览器和模型服务日志都必须隐藏 cookie、完整请求头和完整上游响应。

这一风险不仅影响写入，也影响获取账本、分类、商家、账户、项目和成员等只读操作，因此元数据读取接口同样需要限频、可停止和人工恢复。

### 3.2 获取记账页及全部元数据

```http
GET https://www.sui.com/tally/new.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

这是工程中最重要的读取接口。它返回 HTML，除了记账页面，还包含当前账本下的分类、商家、账户、项目和成员。

页面中还会出现类似配置：

```javascript
window.$param = {
  token: "...",
  defaultCurrency: "CNY",
  accountJson: {...},
  ...
}
```

`token` 是页面动态值，不应永久保存。当前新增账单的前端调用没有显式把它作为 `_token_` 发送；如果以后服务端校验规则变化，再根据实时页面调整。

## 4. 元数据读取说明

随手记为账本设置提供了独立的管理页面。以下页面目前都返回 HTML，不是 JSON 接口；工程适配器应分别请求对应页面并解析，不要把 `tally/new.do` 中为记账表单展示的选项当成管理接口。

| 资源 | 上游页面 | 返回格式 | 主要 ID 来源 |
|---|---|---|---|
| 账本 | `GET /report_index.do` 或 `GET /tally/new.do` | HTML | `data-bookId` |
| 支出分类 | `GET /category/budgetCategory.do` | HTML | `category<ID>` |
| 收入分类 | `GET /category/incomeCategory.do` | HTML | `category<ID>` |
| 商家 | `GET /category/storeCategory.do` | HTML | `category<ID>` |
| 项目 | `GET /category/projectCategory.do` | HTML | `category<ID>` |
| 成员 | `GET /category/member.do` | HTML | `category<ID>` |

这些页面都需要携带当前会话 cookie。每个页面都应单独检查是否被重定向到登录页。

### 4.1 获取账本

上游请求：

```http
GET /report_index.do
GET /tally/new.do
```

解析规则：

| 字段 | HTML 来源 | 说明 |
|---|---|---|
| `id` | `data-bookId` | 账本 ID，写入目标标识 |
| `name` | `title` 或文本内容 | 账本名称 |
| `is_current` | `class` 是否包含 `current` | 页面当前选中的账本 |
| `current_book_id` | `ssjBookSwitch_param.currentBookId` | 页面声明的当前账本 |

建议统一成：

```json
{
  "currentBookId": "2150191014",
  "books": [
    {"id": "2150191014", "name": "自己账本", "isCurrent": true}
  ],
  "fetchedAt": "2026-09-08T00:00:00+08:00"
}
```

写入前的门禁：

```text
页面 currentBookId 对应的账簿名称必须与 SUI_BOOK_NAME 唯一匹配，且当前会话账簿必须是该账簿
```

不相等时直接中止，避免把账记到错误账本。

### 4.2 获取支出/收入分类

支出分类：

```http
GET https://www.sui.com/category/budgetCategory.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

收入分类：

```http
GET https://www.sui.com/category/incomeCategory.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

解析规则：

- 分类行通常是 `div#category<ID>`
- 一级分类使用 `li.li-level1`，二级分类使用 `li.li-level2`
- 分类名称优先读取 `title`，没有时读取去除 HTML 标签后的文本
- 分类 ID 是 `category<ID>` 中最后的数字部分
- 分类统计金额读取该行的 `li.li-tj`
- 真正记账时优先选择二级分类；一级分类只作为父级匹配和展示
- 分类页可能把二级分类行嵌套在一级分类容器内；解析时必须继续识别嵌套的 `div#category<ID>`，不能在遇到第一个嵌套 `</div>` 时提前结束
- 二级分类行可能同时包含一个空的 `li.li-level1` 占位元素和有内容的 `li.li-level2`；应优先读取有可见内容的二级元素，不能只取第一个匹配元素

示例：

```html
<div id="category23551923833" class="lb-row">
  <ul>
    <li class="li-level2" title="食品零食">食品零食</li>
    <li class="li-tj">176689.40</li>
  </ul>
</div>
```

应转换为：

```json
{
  "id": "23551923833",
  "name": "食品零食",
  "type": "expense",
  "level": 2,
  "parentId": "23551923826",
  "parentName": "日常",
  "isLeaf": true
}
```

注意：当前页面存在名称重复的分类，例如不同父分类下可能都有“投资收入”。模型输出名称后，服务端必须结合 `type`、父分类和当前元数据映射 ID，不能只按名称模糊匹配。

### 4.3 获取商家

上游请求：

```http
GET https://www.sui.com/category/storeCategory.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

解析规则：

```css
div[id^="category"]
```

- 商家行通常是 `div#category<ID>`
- 名称优先读取行内带 `title` 的名称元素
- ID 是 `category<ID>` 中最后的数字部分
- “无”或未选择商家在记账请求中使用 `0`

统一格式：

```json
{
  "id": "2147093546",
  "name": "餐馆"
}
```

### 4.4 获取账户名称

账户管理页面不属于分类管理页面，建议使用账户页作为账户名称的正式来源：

```http
GET https://www.sui.com/account/account.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

如果账户页的结构发生变化，也可以从记账页的账户选择器做临时兼容读取，但不应把它作为账户管理接口的长期替代。

记账页兼容解析选择器示例：

```css
#ul_tb-outAccount-1 li[id^="tb-outAccount-1_v_"]
```

也可以读取收入账户选择器 `#ul_tb-inAccount-5`。两者通常展示相同的账户集合，但仍建议分别解析并去重。

解析规则：

- 账户资料必须解析页面中的 `accountNameJson`
- `accountNameJson` 的键是账户 ID，值是账户名称；只有该对象中的键值可以进入账户候选列表
- 不得从账户余额、`a#ll-<ID>`、记账页兼容选择器或其他 HTML 节点补充候选
- 没有解析到 `accountNameJson` 时账户列表为空，账户不可提交

草稿只保存 `accountName`；提交边界再根据账户名称撮合 `accountNameJson` 中的账户 ID。账户列表第一项是默认账户。

### 4.5 获取项目

上游请求：

```http
GET https://www.sui.com/category/projectCategory.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

解析规则：

```css
div[id^="category"]
```

- 项目行通常是 `div#category<ID>`
- 名称优先读取行内带 `title` 的名称元素
- ID 是 `category<ID>` 中最后的数字部分
- “无”或未选择项目使用 `0`

### 4.6 获取成员

上游请求：

```http
GET https://www.sui.com/category/member.do
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

解析规则：

```css
div[id^="category"]
```

- 成员行通常是 `div#category<ID>`
- 名称优先读取行内带 `title` 的名称元素
- ID 是 `category<ID>` 中最后的数字部分
- “无”或未选择成员使用 `0`

### 4.7 建议的统一元数据响应

本项目内部不要让上层业务直接依赖 HTML，建议适配器转换成：

```json
{
  "currentBook": {
    "id": "2150191014",
    "name": "自己账本"
  },
  "books": [],
  "categories": {
    "expense": [],
    "income": []
  },
  "stores": [],
  "accounts": [],
  "projects": [],
  "members": [],
  "currency": "CNY",
  "fetchedAt": "2026-09-08T00:00:00+08:00"
}
```

建议每次准备写入时刷新一次；如果做缓存，缓存失效时间不宜过长，并在分类或账户匹配失败时强制刷新。

## 5. 查询流水接口

### 5.1 查询流水

```http
POST https://www.sui.com/tally/new.rmi
Content-Type: application/x-www-form-urlencoded
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

核心参数：

```text
opt=list2
beginDate=2026.09.01
endDate=2026.09.08
cids=0
bids=0
sids=0
pids=0
memids=0
order=
isDesc=0
page=1
note=
mids=0
```

说明：

- `cids`：分类筛选
- `bids`：账户筛选，不是账本 ID
- `sids`：商家筛选
- `pids`：项目筛选
- `memids`：成员筛选
- `mids`：流水类型筛选
- 当前账本仍由会话状态决定，不由 `bids` 决定

返回结构：

```json
{
  "income": 0.0,
  "payout": 0.0,
  "beginDate": "2026.09.01",
  "endDate": "2026.09.07",
  "pageCount": 1,
  "pageNo": 1,
  "symbol": "¥",
  "groups": [
    {
      "income": 0.0,
      "payout": 0.0,
      "list": []
    }
  ]
}
```

流水记录中目前观察到的字段包括：

```text
sId, tranId, tranType, tranName, itemAmount,
categoryId, categoryName, buyerAcountId, buyerAcount,
sellerAcountId, sellerAcount, projectId, projectName,
memberId, memberName, transferStoreId, memo, url, date
```

这个接口既用于查询，也用于写入后的最终结果核对。

## 6. 新增支出/收入接口

### 6.1 新增支出

```http
POST https://www.sui.com/tally/payout.rmi
Content-Type: application/x-www-form-urlencoded
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

### 6.2 新增收入

```http
POST https://www.sui.com/tally/income.rmi
Content-Type: application/x-www-form-urlencoded
Cookie: 由桥接器内部 CookieJar 自动附带当前会话
```

通用表单字段：

| 字段 | 必填 | 说明 |
|---|---:|---|
| `id` | 是 | 新增使用 `0` |
| `category` | 支出/收入必填 | 分类 ID |
| `store` | 否 | 商家 ID，无则 `0` |
| `time` | 是 | `YYYY-MM-DD HH:mm` |
| `project` | 否 | 项目 ID，无则 `0` |
| `member` | 否 | 成员 ID，无则 `0` |
| `memo` | 否 | 备注 |
| `url` | 否 | 图片地址，无则空字符串 |
| `out_account` | 否 | 普通收支使用 `0` |
| `in_account` | 否 | 普通收支使用 `0` |
| `debt_account` | 否 | 普通收支为空 |
| `account` | 是 | 普通收支的实际账户 ID |
| `price` | 是 | 金额，正数 |
| `price2` | 否 | 多币种或特殊流水使用，普通收支为空 |

支出示例：

```text
id=0
category=<expense_category_id>
store=<store_id_or_0>
time=2026-09-08 19:06
project=<project_id_or_0>
member=<member_id_or_0>
memo=午饭
url=
out_account=0
in_account=0
debt_account=
account=<payment_account_id>
price=35.00
price2=
```

收入示例：

```text
id=0
category=<income_category_id>
store=0
time=2026-09-08 19:07
project=0
member=0
memo=工资
url=
out_account=0
in_account=0
debt_account=
account=<income_account_id>
price=10000.00
price2=
```

## 7. 响应与写入安全

当前前端对响应的主要判断：

| 响应特征 | 含义 | 处理方式 |
|---|---|---|
| `id == -999` | 请求被拒绝 | 记录 `errorInfo`，人工处理 |
| `id == -1` | 会话失效 | 标记 cookie 过期，不重试写入 |
| `id == 0` | 新增失败 | 查询确认后再决定是否人工重试 |
| 其它成功分支 | 前端按新增成功处理 | 仍必须执行流水回读核对 |

普通收支成功时，随手记当前返回的 `id` 是对象而不是数值，例如：

```json
{"id":{"id":21693739113481},"budget":0,"price":1234.5}
```

桥接器使用 `id.id` 作为随手记记录 ID，并保留响应中的 `budget` 和 `price`；旧版直接返回数值 `id` 的响应仍兼容。响应无法解析或没有有效记录 ID 时，结果必须进入 `uncertain`，不能自动重试。

必须遵守以下流程：

```text
刷新元数据
→ 校验当前账本
→ 校验分类/账户/商家 ID 属于本次刷新结果
→ 提交 payout.rmi 或 income.rmi
→ 查询同日期流水
→ 核对金额、类型、分类、账户、备注
→ 标记最终成功或人工处理
```

超时、连接断开或响应无法解析时，不能自动重试，因为第一次请求可能已经在随手记成功落账。

## 8. 本项目建议提供的内部接口

下面是给 iOS 快捷指令和管理页面使用的内部接口。设置、账簿同步、分类同步和模型服务目录接口已经实现；商家、账户、项目、成员的完整元数据刷新仍按后续适配器工作推进。

### 8.1 管理网页配置

```http
GET /api/v1/settings
PATCH /api/v1/settings
Content-Type: application/json
```

可读写的普通配置为 `sui_book_name`、`llm_provider`、`llm_base_url`、`llm_model` 和 `llm_prompt`。`llm_provider` 可选 `openai`、`openrouter`、`custom`。第一版只支持 OpenAI-compatible Chat Completions，不支持 Responses API 或旧的 `/completions` 文本接口。OpenAI 和 OpenRouter 使用内置 Base URL；`custom` 必须填写 Base URL，服务端会请求 `{base_url}/chat/completions`，并尝试请求 `{base_url}/models`。自定义端点需要支持当前使用的 `response_format.type = "json_schema"`。其中 `llm_prompt` 是用户可编辑的解析补充提示，只作为偏好追加到固定系统规则之前；固定的 JSON Schema、当前账簿分类约束和“无法确定时要求确认”规则不会被覆盖。`llm_api_key` 只允许写入，GET 响应只返回已配置状态，不返回原文。随手记地址 `sui_base_url` 仍只在 TOML 中配置，不通过网页设置接口读写。随手记会话不通过内部设置接口提交，只能使用 `sui-copilot auth login` 建立或更新加密 CookieJar。网页保存后统一原子写入启动时使用的 TOML 文件，并以 owner-only 权限保存；配置项不会写入 SQLite。

旧字段 `openrouter_model`、`openrouter_prompt` 和 `openrouter_api_key` 仍可提交作为兼容别名。旧的 `[openrouter]` TOML 配置仍可读取，网页下次保存时转换为 `[llm]`。切换服务商时必须同时提交新服务商的 API Key；同一服务商下 API Key 为空表示保持原值。

`sui_book_name` 是唯一的账簿配置入口，不接受账簿 ID 配置。连接检查或写入前，服务端会从随手记账簿列表按名称查找：零个结果、多个同名结果和当前会话账簿不匹配都会中止。

### 8.2 模型服务模型列表

```http
GET /api/v1/llm/models
```

`GET /api/v1/llm/models` 只读取当前服务商和 Base URL 对应的 SQLite 缓存，服务重启后仍可恢复模型候选和已保存的模型选择。`GET /api/v1/models` 和旧的 `/api/v1/openrouter/models` 是兼容别名。首次使用或需要更新目录时调用：

```http
POST /api/v1/llm/models/sync
```

服务端使用当前服务商的 API Key 请求 `{base_url}/models`，保存模型的 `id`、`name`、上下文长度、支持参数和同步时间，再返回非敏感模型信息。自定义服务不支持 `/models` 时，同步会失败，但网页仍允许手填模型 ID。缓存按服务商和 Base URL 隔离，不保存 API Key。网页按钮名称为“同步模型列表”。

基础 prompt 可通过以下接口只读预览，默认不需要模型服务 API Key，但必须已有当前账簿元数据：

```http
GET /api/v1/llm/prompt
```

返回的 prompt 由服务端生成，包含当前账簿分类和固定 JSON 输出规则；配置中的 `llm_prompt` 只是补充 prompt，不能编辑或覆盖基础 prompt。旧的 `/api/v1/openrouter/prompt` 仍作为兼容别名。

### 8.3 同步随手记账簿

```http
GET /api/v1/sui/books
```

该接口只读取 SQLite 缓存，返回账簿名称、当前标记和 `updatedAt`，不访问随手记。需要更新账簿列表时调用：

```http
POST /api/v1/sui/books/sync
```

同步接口使用 Cookie 请求 `report_index.do`，解析账簿名称列表后保存到 SQLite。响应只返回名称和当前标记，不返回账簿 ID；账簿 ID 只在服务端内部用于后续上游请求和 SQLite 元数据快照。目标账簿选择也会保存到 SQLite，并记录 `updated_at`。

选择账簿后调用：

```http
POST /api/v1/sui/sync
Content-Type: application/json

{"bookName":"自己账本"}
```

服务端按名称解析并校验账簿，必要时切换随手记当前账簿，然后请求支出/收入分类页面。账簿和分类结果会保存到 SQLite，供后续记账匹配使用；用户不需要手工配置分类或任何 ID。

### 8.4 获取统一元数据

```http
GET /api/v1/metadata
```

默认行为是读取最近一次同步并保存在 SQLite 的快照，不会因为页面打开而自动请求随手记。账簿和分类通过第 8.3 节的同步接口更新；按 `SUI_BOOK_NAME` 唯一解析目标账簿，并校验当前账本是否为该账簿后，返回第 4.7 节的统一 JSON。

### 8.5 接收快捷指令输入

```http
POST /api/v1/ingest
Content-Type: application/json
```

请求：

```json
{
  "source": "ios-shortcut",
  "inputType": "text",
  "text": "今天中午在餐馆吃饭35元，账户记到账户",
  "clientRequestId": "uuid"
}
```

服务端先把输入文本和空草稿保存为 `queued` 状态并立即返回 `202 Accepted`，管理页面将其显示为“待解析”；之后由后台读取最近同步的元数据，确认它属于配置的 `sui_book_name`，再把当前账簿的支出/收入二级分类路径、账户、商家、项目和成员名称作为上下文发送给当前选择的模型服务。模型只返回结构化草稿；分类字段必须是当前账簿中的二级分类路径，账户未指定时使用账户列表第一项。`date`、`amount`、`categoryName`、`accountName` 是必填字段，其他字段可以为空。模型返回的 `date` 只要严格符合 `YYYY-MM-DD HH:mm` 且包含合法小时和分钟，即使原文只有日期或模糊时段，也不因时间来源而进入 `needs_confirmation`；不能用 `00:00` 或其他默认时间冒充无法确定的时间。

解析结果完整且明确时，后台会直接进入随手记提交流程，不再要求再次点击确认；提交成功进入 `committed`，随手记拒绝进入 `rejected`，写入结果无法确定进入 `uncertain`。后台记账流程按记录进入顺序使用单工 FIFO 队列，同一时刻只处理一笔，避免多笔记录并行调用模型和随手记接口；人工确认提交也与后台自动提交共享随手记写入闸门。只有字段缺失、模型明确标记不确定或本地校验无法确认时才进入 `needs_confirmation`，由用户手动补充和确认。Debug 模式保留提交预览限制，不会自动实际写入。

后台单笔处理总时限为 10 分钟。每次随手记请求另有 30 秒时限，包括该请求触发的 SSO 恢复；调用方更短的时限仍然有效。提交结束后，本地状态保存使用独立的 5 秒时限，不因上游超时或调用方断开而放弃保存结果。明确成功、明确拒绝和结果不确定分别保存为 `committed`、`rejected` 和 `uncertain`；后续队列继续处理，不自动重发记账请求。数据库或磁盘故障仍可能导致保存失败，此时服务记录错误，残留的 `committing` 在下次启动时转为待核对。

输入文本会去掉首尾空白后保存。同一 `inputType` 和输入文本已有本地记录时，返回 `409 duplicate_entry`，响应包含已存在的 `entryId`；重复检查在 AI 调用之前执行，因此不会进一步解析，也不会创建新记录。重新解析已有记录时会排除当前记录自身，但如果另一条记录已有相同输入，仍返回相同的重复错误。

后台解析如果没有同步当前账簿资料，会把记录转为 `rejected` 并保存 `category_metadata_not_synced`；如果资料属于其他账簿，会保存 `category_metadata_book_mismatch`；如果没有 API Key，会保存 `llm_not_configured`；模型配置无效会保存 `llm_config_invalid`；模型请求失败会保存 `llm_parse_failed`，同时在 `errorMessage` 中保留安全的具体诊断（例如网络错误、模型服务 HTTP 状态码及上游 `error.message`、响应格式异常或模型内容不是有效草稿）。服务重启时，无法继续的 `processing` 记录会转为 `rejected`，保存 `processing_interrupted`，并记录“服务重启导致解析中断，请重新解析”；遗留的 `committing` 记录则转为 `uncertain`，保存 `sui_commit_interrupted`，保留原草稿并要求先核对随手记流水，不把未完成请求当成成功或自动重放。不会返回完整上游响应、请求头或 API Key。页面选择失败记录时会展示该诊断和错误代码。这些情况都不会创建一个由本地规则猜出的替代草稿，用户可在记录详情中重新提交处理。

解析过程中也可以通过页面的“中断解析”按钮或以下接口手动阻断当前记录，不需要重启服务：

```http
POST /api/v1/entries/{id}/interrupt
```

该接口只接受 `processing` 状态。服务先以状态条件将记录转为 `rejected`，错误代码为 `processing_cancelled`，再取消当前模型请求；这样即使上游请求稍后返回，也不会继续保存解析结果或自动提交。中断后的记录可以使用“重新解析”再次进入 `queued`。

记录详情中的“重新解析”调用：

```http
POST /api/v1/entries/{id}/reparse
Content-Type: application/json

{"text":"修改后的原始记账描述"}
```

它只允许用于 `pending_submission`、`needs_confirmation`、`rejected` 或 `uncertain` 记录，使用请求中的文本按版本覆盖原始输入和草稿并立即回到 `queued`；后台再使用当前账簿资料和当前解析补充提示重新解析。页面不会直接展示内部状态值，当前映射为：`queued` 待解析、`processing` 解析中、`pending_submission` 待提交、`needs_confirmation` 待人工确认、`committing` 提交中、`committed` 已记账、`rejected` 提交失败、`uncertain` 待核对。

不确定记录的核对调用：

```http
POST /api/v1/entries/{id}/reconcile
POST /api/v1/entries/reconcile-uncertain
```

页面点击“刷新并核对”时会批量调用第二个接口；服务后台每 5 分钟也会执行一次。核对只读取对应日期的随手记流水，不会重试写入：找到唯一匹配时自动标记为 `committed`；一次有效核对没有唯一匹配时保留 `uncertain` 并记录次数；连续两次没有唯一匹配时转为 `needs_confirmation`，要求人工处理。普通 `GET /api/v1/entries` 刷新列表本身不会访问随手记。

后台维护每天凌晨 2:00（Asia/Shanghai）删除 `updated_at` 超过 7 天的 `committed` 本地记录及其状态事件，并执行 SQLite `VACUUM`；不确定和待人工处理记录不会被该任务删除。

删除记录调用：

```http
DELETE /api/v1/entries/{id}
```

删除会同时删除本地记录及其状态事件，不会新增其他记录状态。

### 8.6 Debug 提交预览

以下接口只有服务使用 `--debug` 启动时可用：

```http
GET /api/v1/debug
POST /api/v1/entries/{id}/submit-preview
POST /api/v1/entries/{id}/submit-preview/execute
```

`submit-preview` 只生成实际随手记提交的 method、URL、headers 和 form payload，不发送请求。`execute` 接收可编辑的预览对象后发送一次请求，并返回上游 status、headers 和原始 body；失败响应也以 HTTP 200 的预览结果返回，便于页面展示。后台固定 URL 必须属于当前随手记地址、method 必须为 `POST`、路径必须与支出/收入类型一致；Cookie 和 Authorization 不接受前台覆盖。

### 8.7 预览和结构化解析（后续接口设计）

```http
POST /api/v1/entries/preview
Content-Type: application/json
```

请求：

```json
{
  "text": "今天中午在餐馆吃饭35元",
  "timezone": "Asia/Shanghai"
}
```

建议响应：

```json
{
  "requestId": "uuid",
  "status": "preview_required",
  "entry": {
    "type": "expense",
    "date": "2026-09-08",
    "amount": 35.0,
    "categoryName": "食品零食",
    "accountName": "账户",
    "storeName": "餐馆",
    "projectName": "无",
    "memberName": "无",
    "memo": "午饭"
  },
  "errors": [],
  "warnings": []
}
```

模型服务只负责把自然语言转换成名称和标准字段，不输出 `categoryId`。服务端根据当前账簿元数据把 AI 返回的分类路径解析为唯一分类 ID，保存到草稿的内部 `categoryId` 字段；最终随手记写入只把该 ID 传给写入适配器，不传分类名称。

### 8.8 提交记账

```http
POST /api/v1/entries/commit
Content-Type: application/json
```

请求：

```json
{
  "requestId": "uuid",
  "confirm": true
}
```

服务端职责：

1. 重新读取或确认元数据未过期。
2. 确认当前账本 ID。
3. 把标准对象映射为随手记表单字段。
4. 调用上游写入接口。
5. 用 `new.rmi` 查询并核对最终流水。
6. 返回 `committed`、`rejected` 或 `uncertain`。

### 8.9 查询本地处理状态

```http
GET /api/v1/entries/{requestId}
```

建议状态：

```text
received
parsed
preview_required
committing
committed
rejected
uncertain
manual_action_required
```

`uncertain` 和 `manual_action_required` 状态禁止自动重试。

## 9. 模型服务输出约束

OpenAI、OpenRouter 和自定义 OpenAI-compatible 服务都必须支持当前 Chat Completions 请求中的 JSON Schema 结构化输出。模型只输出以下结构，不允许输出任意上游请求参数或 ID：

```json
{
  "type": "expense",
  "date": "YYYY-MM-DD HH:mm",
  "amount": 0.0,
  "categoryName": "",
  "accountName": "",
  "storeName": "",
  "projectName": "无",
  "memberName": "无",
  "memo": "",
  "confidence": 0.0,
  "needsConfirmation": false
}
```

本地校验至少包括：

- `type` 只能是 `expense` 或 `income`
- `amount` 必须大于 0，且金额精度限制为两位小数
- 日期必须是合法日期
- 日期必须严格为 `YYYY-MM-DD HH:mm`，并包含合法的小时和分钟；是否由模型根据日期或模糊时段推断，不再单独触发 `needs_confirmation`
- 分类必须匹配当前类型的叶子分类路径
- 账户必须存在于当前账本元数据；未指定时使用第一项
- `date`、`amount`、`categoryName`、`accountName` 缺失时禁止提交
- 商家、项目、成员、备注可以为空；空值提交时由上游协议转换为无关联值
- 商家、项目和成员优先匹配当前账簿列表中的原名；用户使用明确且唯一的同义说法或场景描述时，应选择对应原名，不能创造列表外名称；只有没有候选或存在歧义时才留空
- 原文直接出现当前成员列表中的名称时，必须填写该成员原名；“我”或“本人”在成员列表存在“本人”时匹配为“本人”。明确用餐且没有外卖、打包、配送等外带信号时，若商家列表存在“堂食”，商家默认匹配为“堂食”；有明确外带/配送信号时再匹配对应渠道
- `memo` 在其他字段抽取完成后生成，只保留未被字段吸收的商品/餐品、地点或必要上下文；必须剔除时间、金额、人员、商家、账户、分类、项目和完整动作句，不得把原始整句复制进去。对于“[时间][成员]在[地点][商家场景]进行了[动作][商品/餐品]并支付[金额]”这类句式，应把成员和商家场景写入对应字段，并将备注压缩为“[地点][商品/餐品]”
- 未明示姓名或人物指代时，成员留空；省略主语不能理解为“本人”。商家字段为“堂食”“外卖”等渠道时，具体店名仍需保留在备注。例如“今天中午在小巴依吃了50块钱”，商家为“堂食”、成员为空、备注为“小巴依”。
- 模型返回后，服务端会使用本次原始输入和当前元数据再次执行成员原名、第一人称和用餐默认商家的确定性归一化，并清除没有第一人称依据的“本人”；备注清理只删除实际选中的名称，不按整个候选列表删除。这不是新增列表项，也不替代模型对分类和备注的解析

## 10. 暂不纳入第一版的接口

以下能力已经在网页脚本中看到，但暂不建议第一版实现：

- 转账：`POST /tally/transfer.rmi`
- 借入/借出/还款/收债等债务类流水
- 图片上传：`new.do?opt=upload`
- 自动切换账本：`/systemSet/book.do?opt=switch&switchId=...`
- 新增分类、商家、项目、成员或账户

自动切换账本会改变会话状态，属于高风险操作。第一版应在账本不匹配时停止，而不是自行切换。

## 11. 工程验收清单

- [ ] 能通过 `auth login` 建立会话并获取 `tally/new.do`
- [ ] 能解析当前账本和账本列表
- [ ] 能解析支出/收入分类及父子关系
- [ ] 能解析商家、账户、项目、成员的真实 ID
- [ ] 能执行只读流水查询 `new.rmi?opt=list2`
- [ ] 能在写入前校验账本和所有 ID
- [ ] 能区分上游拒绝、会话失效、成功和不确定状态
- [ ] 能写入后回读并核对流水
- [ ] 不会自动重试不确定写入
- [ ] cookie、模型服务 API Key 和完整上游响应不会进入普通日志
- [ ] iOS 快捷指令能通过 HTTPS 或可信内网访问 `/api/v1/ingest`
