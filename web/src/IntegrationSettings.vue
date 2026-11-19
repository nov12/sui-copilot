<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api } from './api'
import CodeExample from './CodeExample.vue'
import { copyTextToClipboard } from './clipboard'

const publicUrl = ref<string | null>(null)
const address = ref('')
const addressBusy = ref(false)
const addressMessage = ref('')
const addressError = ref(false)
const protocol = ref<'http' | 'mcp'>('http')
const mcpCopyFeedback = ref('')

const addressChanged = computed(() => address.value.trim() !== publicUrl.value)
const baseUrl = computed(() => publicUrl.value || window.location.origin)
const mcpUrl = computed(() => `${baseUrl.value}/mcp`)

onMounted(loadAddress)
watch(mcpUrl, () => { mcpCopyFeedback.value = '' })

async function loadAddress() {
  addressBusy.value = true
  addressMessage.value = ''
  addressError.value = false
  try {
    const result = await api.getSettings()
    const savedAddress = result.settings.server_public_url ?? ''
    publicUrl.value = savedAddress
    address.value = savedAddress
  } catch (error) {
    addressError.value = true
    addressMessage.value = error instanceof Error ? error.message : '读取对外访问地址失败。'
  } finally {
    addressBusy.value = false
  }
}

async function saveAddress() {
  if (addressBusy.value || !addressChanged.value) return
  addressBusy.value = true
  addressMessage.value = ''
  addressError.value = false
  try {
    const result = await api.updateSettings({ server_public_url: address.value.trim() })
    const savedAddress = result.settings.server_public_url
    publicUrl.value = savedAddress
    address.value = savedAddress
    addressMessage.value = savedAddress ? '地址已保存，示例已更新。' : '已清空。示例现在使用当前网页地址。'
  } catch (error) {
    addressError.value = true
    addressMessage.value = error instanceof Error ? error.message : '保存对外访问地址失败。'
  } finally {
    addressBusy.value = false
  }
}

async function copyMcpUrl() {
  const value = mcpUrl.value
  const copied = await copyTextToClipboard(value)
  if (value === mcpUrl.value) mcpCopyFeedback.value = copied ? '已复制' : '复制失败，可以手动选择地址复制。'
}

const structuredEntry = JSON.stringify({
  clientRequestId: '替换为每笔唯一的请求 ID',
  bookId: '替换为当前账簿 ID',
  draft: {
    type: 'expense',
    date: 'YYYY-MM-DD HH:mm',
    amount: '35.00',
    categoryName: '餐饮 / 午餐',
    accountName: '银行卡',
    needsConfirmation: true,
  },
}, null, 2)

const textEntry = JSON.stringify({
  source: 'ios-shortcut',
  inputType: 'text',
  text: '今天午饭花了 35 元',
}, null, 2)

const endpoints = [
  {
    method: 'GET', path: '/api/v1/health', title: '测试连通性',
    description: '返回服务状态和运行版本。',
  },
  {
    method: 'GET', path: '/api/v1/metadata', title: '读取账簿资料',
    description: '取得 bookId、二级分类和账户。首次使用请登录随手记并同步目标账簿。',
  },
  {
    method: 'POST', path: '/api/v1/entries', title: '提交结构化账目', body: structuredEntry,
    description: '替换账簿 ID、二级分类路径、账户、北京时间和每笔唯一的请求 ID。金额使用最多两位小数的正数字符串。',
    result: '首次接收返回 202 和 entryId。测试时保留 needsConfirmation: true，记录会留在网页待人工确认。正式自动记账时省略此字段。请求超时后使用原请求 ID 和原参数重试。',
  },
  {
    method: 'GET', path: '/api/v1/entries/{id}', title: '查询处理状态', examplePath: '/api/v1/entries/42',
    description: '将 42 换成提交返回的 entryId。entry.state 为 committed 时表示已入账，为 uncertain 时请先核对随手记流水。',
  },
  {
    method: 'POST', path: '/api/v1/ingest', title: '文本记账', body: textEntry,
    description: '供脚本和快捷指令使用。配置账簿资料和模型服务后，解析结果明确时会自动尝试入账。',
    result: '首次接收返回 202，通过 entry.id 查询处理状态。',
  },
]

function shellQuote(value: string): string {
  return "'" + value.replaceAll("'", "'\\''") + "'"
}

const httpEndpoints = computed(() => endpoints.map(endpoint => {
  const url = shellQuote(baseUrl.value + (endpoint.examplePath ?? endpoint.path))
  let command = `curl --globoff -sS ${url}`
  if (endpoint.body) {
    command = [
      command + ' \\',
      "  -H 'Content-Type: application/json' \\",
      `  --data ${shellQuote(endpoint.body)}`,
    ].join('\n')
  }
  const examples = [{ label: 'cURL', text: command }]
  if (endpoint.body) examples.push({ label: 'JSON 请求体', text: endpoint.body })
  return { ...endpoint, examples }
}))

const mcpExamples = computed(() => [
  { label: 'Codex · config.toml', text: `[mcp_servers.sui-copilot]\nurl = ${JSON.stringify(mcpUrl.value)}` },
  { label: 'Claude Code · CLI', text: `claude mcp add --transport http sui-copilot ${shellQuote(mcpUrl.value)}` },
])

const mcpTools = [
  { name: 'get_book_metadata', title: '读取账簿资料', description: '连接后先调用。返回账簿 ID、北京时间、分类路径和账户等可选值。', text: '{}' },
  { name: 'submit_entry', title: '提交结构化账目', description: '参数与 HTTP 提交一致，返回 entryId。测试时保留 needsConfirmation: true。', text: structuredEntry },
  { name: 'get_entry_status', title: '查询处理状态', description: '将 42 换成提交返回的 entryId。返回处理状态及错误原因。', text: '{"entryId":42}' },
]
</script>

<template>
  <section class="integration-settings" aria-label="接口接入">
    <div v-if="publicUrl === null" class="config-group address-loading">
      <p class="field-hint" role="status">{{ addressBusy ? '正在读取对外访问地址…' : addressMessage }}</p>
      <button v-if="!addressBusy" type="button" class="secondary compact-action" @click="loadAddress">重新读取</button>
    </div>
    <template v-else>
      <form class="config-group form-field" novalidate @submit.prevent="saveAddress">
        <label for="public-url" class="field-label">对外访问地址</label>
        <div class="address-row">
          <input id="public-url" v-model="address" type="url" inputmode="url" spellcheck="false" autocomplete="off" placeholder="https://book.example.com" aria-describedby="public-url-help" :disabled="addressBusy" @input="addressMessage = ''; addressError = false">
          <button type="submit" :disabled="addressBusy || !addressChanged">{{ addressBusy ? '保存中…' : '保存地址' }}</button>
        </div>
        <p id="public-url-help" class="field-hint">HTTP API 和 MCP 共用此地址。留空时使用当前网页地址。</p>
        <p v-if="addressMessage || addressChanged" class="field-hint" :class="{ 'address-error': addressError }" role="status">{{ addressMessage || '保存后更新下方示例。' }}</p>
      </form>

      <div class="protocol-tabs" role="group" aria-label="接口类型">
        <button type="button" aria-controls="http-api-examples" :aria-pressed="protocol === 'http'" @click="protocol = 'http'">HTTP API</button>
        <button type="button" aria-controls="mcp-examples" :aria-pressed="protocol === 'mcp'" @click="protocol = 'mcp'">MCP</button>
      </div>

      <section v-show="protocol === 'http'" id="http-api-examples" class="route-list" aria-label="HTTP API">
        <details v-for="endpoint in httpEndpoints" :key="`${endpoint.method} ${endpoint.path}`" class="integration-route" :open="endpoint.path === '/api/v1/health'">
          <summary>
            <code class="route-method">{{ endpoint.method }}</code>
            <code class="route-path">{{ endpoint.path }}</code>
            <span class="field-hint route-title">{{ endpoint.title }}</span>
          </summary>
          <div class="route-body">
            <p class="field-hint">{{ endpoint.description }}</p>
            <CodeExample :examples="endpoint.examples" :copy-label="endpoint.body ? '复制示例' : '复制命令'" />
            <p v-if="endpoint.result" class="field-hint">{{ endpoint.result }}</p>
          </div>
        </details>
      </section>

      <section v-show="protocol === 'mcp'" id="mcp-examples" class="integration-settings" aria-label="MCP">
        <div class="config-group mcp-address">
          <div class="form-field">
            <span class="field-hint">MCP 地址 · Streamable HTTP</span>
            <code>{{ mcpUrl }}</code>
            <p v-if="mcpCopyFeedback" class="field-hint" role="status">{{ mcpCopyFeedback }}</p>
          </div>
          <button type="button" class="secondary compact-action" @click="copyMcpUrl">复制地址</button>
        </div>
        <CodeExample :examples="mcpExamples" copy-label="复制配置" select-label="MCP 客户端" />
        <div class="route-list">
          <details v-for="tool in mcpTools" :key="tool.name" class="integration-route mcp-tool">
            <summary><code class="route-path">{{ tool.name }}</code><span class="field-hint route-title">{{ tool.title }}</span></summary>
            <div class="route-body">
              <p class="field-hint">{{ tool.description }}</p>
              <CodeExample :examples="[{ label: '工具参数', text: tool.text }]" copy-label="复制参数" />
            </div>
          </details>
          <details class="integration-route caddy-guide">
            <summary>通过 Caddy 接入</summary>
            <div class="route-body">
              <p class="field-hint">Caddy 转发 <code>/mcp</code> 到回环监听地址时，上游 <code>Host</code> 使用回环地址，例如 <code>127.0.0.1:8080</code>。网页和 HTTP API 透传原始 <code>Host</code>。</p>
              <p class="field-hint">使用路径前缀时，让 Caddy 去掉该前缀再转发。跨设备访问时，在代理上配置 HTTPS 和访问认证。</p>
            </div>
          </details>
        </div>
      </section>
    </template>
  </section>
</template>

<style scoped>
.integration-settings { display: grid; gap: 12px; min-width: 0; }
.address-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 8px; }
.address-row input { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.address-row button { white-space: nowrap; }
.address-loading, .mcp-address { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.address-loading button, .mcp-address button { flex-shrink: 0; }
.address-error { color: #fda4af; }
.mcp-address code, .route-path { overflow-wrap: anywhere; }
.protocol-tabs { display: flex; gap: 4px; width: fit-content; padding: 3px; border: 1px solid #334155; border-radius: 10px; background: #0f172a; }
.protocol-tabs button { padding: 7px 12px; color: #94a3b8; background: transparent; font-size: .82rem; }
.protocol-tabs button[aria-pressed='true'] { color: #e2e8f0; background: #334155; }
.route-list { display: grid; gap: 8px; min-width: 0; }
.integration-route { min-width: 0; border: 1px solid #334155; border-radius: 10px; }
.integration-route summary { display: grid; grid-template-columns: 42px minmax(0, 1fr) auto 10px; align-items: center; gap: 4px 8px; padding: 10px 12px; list-style: none; cursor: pointer; }
.integration-route summary::-webkit-details-marker { display: none; }
.integration-route summary::after { content: ''; width: 6px; height: 6px; border-right: 1.5px solid #94a3b8; border-bottom: 1.5px solid #94a3b8; transform: rotate(-45deg); }
.integration-route[open] summary::after { transform: rotate(45deg); }
.route-method { color: #7dd3fc; font-size: .75rem; font-weight: 650; }
.route-path { font-size: .82rem; }
.route-body { display: grid; gap: 8px; min-width: 0; padding: 0 12px 12px; }
.mcp-tool summary { grid-template-columns: minmax(0, 1fr) auto 10px; }
.caddy-guide summary { grid-template-columns: minmax(0, 1fr) 10px; font-size: .85rem; }
@media (max-width: 560px) {
  .integration-route:not(.mcp-tool, .caddy-guide) summary { grid-template-columns: 36px minmax(0, 1fr) 10px; }
  .mcp-tool summary { grid-template-columns: minmax(0, 1fr) 10px; }
  .integration-route:not(.caddy-guide) summary::after { grid-column: -2; grid-row: 1 / span 2; }
  .route-title { grid-column: 2; grid-row: 2; }
  .mcp-tool .route-title { grid-column: 1; }
}
</style>
