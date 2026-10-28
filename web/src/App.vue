<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { api, type BookOption, type CategoryOption, type Draft, type Entry, type ModelOption, type SecretStatus, type Settings, type SubmitPreview, type SubmitPreviewResult, type SyncedMetadata } from './api'

const text = ref('')
const entries = ref<Entry[]>([])
const manualOnly = ref(false)
const selected = ref<Entry | null>(null)
const editingEntry = ref<Entry | null>(null)
const draft = ref<Draft | null>(null)
const busy = ref(false)
const refreshBusy = ref(false)
const message = ref('')
const settingsOpen = ref(false)
const settingsBusy = ref(false)
const cleanupBusy = ref(false)
const settingsMessage = ref('')
const settingsMessageKind = ref<'success' | 'error' | 'info'>('info')
const deleteConfirmOpen = ref(false)
const basePrompt = ref('')
const basePromptOpen = ref(false)
const basePromptBusy = ref(false)
const basePromptMessage = ref('')
const settingsMetadataExpanded = ref(false)
const pageMetadataExpanded = ref(false)
const debugEnabled = ref(false)
const submitPreviewData = ref<SubmitPreview | null>(null)
const submitPreviewBusy = ref(false)
const submitPreviewHeaders = ref('')
const submitPreviewResult = ref<SubmitPreviewResult | null>(null)
const submitFeedbackState = ref<'idle' | 'processing' | 'success' | 'error'>('idle')
const submitFeedbackError = ref('')
const detailElement = ref<HTMLElement | null>(null)
const detailClosing = ref(false)
const secretStatus = ref<SecretStatus>({ llm_api_key: false })
const interruptConfirmOpen = ref(false)
const models = ref<ModelOption[]>([])
const modelCatalogUpdatedAt = ref('')
const modelQuery = ref('')
const modelPickerActive = ref(false)
const books = ref<BookOption[]>([])
const booksUpdatedAt = ref('')
const syncedMetadata = ref<SyncedMetadata | null>(null)
const settingsForm = reactive<Settings>({ sui_book_name: '', cleanup_retention_days: '7', llm_provider: 'openrouter', llm_base_url: '', llm_model: '', llm_prompt: '' })
const secretForm = reactive({ llm_api_key: '' })
const savedProvider = ref<Settings['llm_provider']>('openrouter')
let refreshTimer: number | undefined
let refreshInFlight = false

const cleanupRetentionOptions = [
  { value: '1', label: '1 天' },
  { value: '2', label: '2 天' },
  { value: '7', label: '7 天' },
  { value: '15', label: '15 天' },
  { value: '30', label: '30 天' },
  { value: '90', label: '90 天' },
  { value: '0', label: '永久' },
]

const stateLabels: Record<string, string> = {
  queued: '待解析',
  processing: '解析中',
  pending_submission: '待提交',
  needs_confirmation: '待人工确认',
  committing: '提交中',
  committed: '已入账',
  rejected: '提交失败',
  uncertain: '待核对',
}
const parseFailureCodes = new Set(['category_metadata_not_synced', 'category_metadata_book_mismatch', 'openrouter_not_configured', 'llm_not_configured', 'llm_config_invalid', 'llm_parse_failed', 'draft_encode_failed', 'invalid_draft', 'category_not_found', 'category_not_leaf', 'category_name_ambiguous', 'account_not_found', 'processing_cancelled', 'processing_timeout'])

function stateLabel(state: string): string {
  return stateLabels[state] ?? '未知状态'
}

function entryStateLabel(entry: Entry): string {
  if (entry.state === 'rejected' && entry.errorCode && parseFailureCodes.has(entry.errorCode)) return '解析失败'
  return stateLabel(entry.state)
}

function stateClass(state: string): string {
  return `state-${state}`
}

function isBackgroundState(state: string): boolean {
  return ['queued', 'processing', 'committing'].includes(state)
}

function canEditEntryState(state: string): boolean {
  return !['queued', 'processing', 'committing', 'committed'].includes(state)
}

type CategoryGroup = { parent: CategoryOption; children: CategoryOption[] }

const selectedState = computed(() => selected.value?.state ?? '')
const selectedStateLabel = computed(() => selected.value ? entryStateLabel(selected.value) : stateLabel(selectedState.value))
const visibleEntries = computed(() => manualOnly.value ? entries.value.filter(entry => ['pending_submission', 'needs_confirmation', 'rejected', 'uncertain'].includes(entry.state)) : entries.value)
const editing = computed(() => editingEntry.value !== null)
const modelMatches = computed(() => {
  const query = modelQuery.value.trim().toLocaleLowerCase()
  const isCurrentValue = modelPickerActive.value && query === settingsForm.llm_model.trim().toLocaleLowerCase()
  if (!query || isCurrentValue) return models.value.slice(0, 50)
  return models.value.filter(model => `${model.name} ${model.id}`.toLocaleLowerCase().includes(query)).slice(0, 50)
})
const modelInput = computed({
  get: () => modelPickerActive.value ? modelQuery.value : settingsForm.llm_model,
  set: (value: string) => { modelPickerActive.value = true; modelQuery.value = value },
})
const ledgerReady = computed(() => {
  const metadata = syncedMetadata.value
  const bookName = settingsForm.sui_book_name.trim()
  return Boolean(metadata && bookName && metadata.currentBook.name.trim() === bookName)
})
const expenseCategoryGroups = computed(() => buildCategoryGroups(syncedMetadata.value?.categories.expense ?? []))
const incomeCategoryGroups = computed(() => buildCategoryGroups(syncedMetadata.value?.categories.income ?? []))
const categoryOptions = computed(() => {
  if (!draft.value || !syncedMetadata.value) return []
  const items = draft.value.type === 'income' ? syncedMetadata.value.categories.income : syncedMetadata.value.categories.expense
  return items.filter(isLeafCategory)
})
const selectedCategoryKnown = computed(() => {
  const name = draft.value?.categoryName?.trim()
  return Boolean(name && categoryOptions.value.some(item => name === item.name.trim() || name === categoryLabel(item)))
})
const categoryReady = computed(() => selectedCategoryKnown.value)
const amountValid = computed(() => {
  const amount = String(draft.value?.amount ?? '').trim()
  return /^[0-9]+(?:\.[0-9]{1,2})?$/.test(amount) && Number(amount) > 0
})
const requiredFieldsReady = computed(() => {
  const currentDraft = draft.value
  const date = currentDraft?.date?.trim() ?? ''
  const accountName = currentDraft?.accountName?.trim() ?? ''
  if (!currentDraft || !date || !amountValid.value || !accountName || !categoryReady.value) return false
  return Boolean(syncedMetadata.value?.accounts.some(item => item.name.trim() === accountName))
})
const submitPreviewResultText = computed(() => submitPreviewResult.value ? JSON.stringify(submitPreviewResult.value, null, 2) : '')

watch(draft, () => {
  submitPreviewData.value = null
  submitPreviewResult.value = null
  submitPreviewHeaders.value = ''
}, { deep: true })
const pad2 = (value: number) => String(value).padStart(2, '0')

function isLeafCategory(item: CategoryOption): boolean {
  return item.isLeaf === true || item.level === 2 || Boolean(item.parentId)
}

function buildCategoryGroups(items: CategoryOption[]): CategoryGroup[] {
  const groups: CategoryGroup[] = []
  const groupsByID = new Map<string, CategoryGroup>()
  for (const item of items) {
    if (item.parentId) continue
    const group = { parent: item, children: [] }
    groups.push(group)
    groupsByID.set(item.id, group)
  }
  for (const item of items) {
    if (!item.parentId) continue
    let group = groupsByID.get(item.parentId)
    if (!group) {
      group = { parent: { id: item.parentId, name: item.parentName || '未命名一级分类', level: 1, isLeaf: false }, children: [] }
      groups.push(group)
      groupsByID.set(item.parentId, group)
    }
    group.children.push(item)
  }
  return groups
}

function categoryLabel(item: CategoryOption): string {
  return item.parentName ? `${item.parentName} / ${item.name}` : item.name
}

function formatBrowserDateTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  const year = date.getFullYear()
  const month = pad2(date.getMonth() + 1)
  const day = pad2(date.getDate())
  const hour = pad2(date.getHours())
  const minute = pad2(date.getMinutes())
  const second = pad2(date.getSeconds())
  const offsetMinutes = -date.getTimezoneOffset()
  const sign = offsetMinutes >= 0 ? '+' : '-'
  const totalMinutes = Math.abs(offsetMinutes)
  const offsetHour = Math.trunc(totalMinutes / 60)
  const offsetMinute = totalMinutes % 60
  const timezoneLabel = offsetMinute === 0 ? `GMT${sign}${offsetHour}` : `GMT${sign}${offsetHour}:${pad2(offsetMinute)}`
  return `${year}/${month}/${day} ${hour}:${minute}:${second} ${timezoneLabel}`
}

function notifySync(kind: 'success' | 'error', text: string) {
  settingsMessageKind.value = kind
  settingsMessage.value = text
}

async function refresh() {
  if (refreshInFlight || document.visibilityState !== 'visible') return
  refreshInFlight = true
  try {
    const result = await api.listEntries()
    entries.value = result.entries
    if (selected.value && !busy.value) {
      const latest = result.entries.find(entry => entry.id === selected.value?.id)
      const shouldSyncSelected = isBackgroundState(selected.value.state) || latest?.state !== selected.value.state || selected.value.state === 'uncertain'
      if (latest && shouldSyncSelected && (latest.state !== selected.value.state || latest.version !== selected.value.version || latest.draftJson !== selected.value.draftJson)) {
        openEntry(latest)
      }
    }
  } finally {
    refreshInFlight = false
  }
}

function startRefreshTimer() {
  if (refreshTimer !== undefined) return
  refreshTimer = window.setInterval(() => { void refresh().catch(() => {}) }, 3000)
}

function stopRefreshTimer() {
  if (refreshTimer !== undefined) {
    window.clearInterval(refreshTimer)
    refreshTimer = undefined
  }
}

async function refreshAndReconcile() {
  if (refreshBusy.value) return
  refreshBusy.value = true
  try {
    await refresh()
    if (!entries.value.some(entry => entry.state === 'uncertain')) {
      message.value = '记录列表已刷新。'
      return
    }
    const result = await api.reconcileUncertain()
    await refresh()
    const details = [`已核对 ${result.checked} 条`, `自动确认 ${result.committed} 条`]
    if (result.needsConfirmation) details.push(`${result.needsConfirmation} 条转为待人工确认`)
    if (result.errors) details.push(`${result.errors} 条暂时无法核对`)
    message.value = `${details.join('，')}。`
  } catch (error) {
    message.value = error instanceof Error ? error.message : '刷新或核对记录失败'
  } finally { refreshBusy.value = false }
}

async function loadBooks() {
  const result = await api.listBooks()
  books.value = result.books
  booksUpdatedAt.value = result.updatedAt ?? ''
}

async function loadModels() {
  const result = await api.llmModels()
  models.value = result.models
  modelCatalogUpdatedAt.value = result.fetchedAt
}

async function loadDebugStatus() {
  try {
    const result = await api.debugStatus()
    debugEnabled.value = result.enabled
  } catch {
    debugEnabled.value = false
  }
}

async function loadSettings() {
  const result = await api.getSettings()
  Object.assign(settingsForm, {
    sui_book_name: result.settings.sui_book_name,
    cleanup_retention_days: result.settings.cleanup_retention_days,
    llm_provider: result.settings.llm_provider,
    llm_base_url: result.settings.llm_base_url,
    llm_model: result.settings.llm_model,
    llm_prompt: result.settings.llm_prompt,
  })
  savedProvider.value = result.settings.llm_provider
  secretStatus.value = result.secrets
}

const providerLabel = computed(() => {
  if (settingsForm.llm_provider === 'openai') return 'OpenAI'
  if (settingsForm.llm_provider === 'custom') return '自定义模型服务'
  return 'OpenRouter'
})
const customProvider = computed(() => settingsForm.llm_provider === 'custom')
const providerBaseURLHint = computed(() => {
  if (settingsForm.llm_provider === 'openai') return '默认：https://api.openai.com/v1'
  if (settingsForm.llm_provider === 'openrouter') return '默认：https://openrouter.ai/api/v1'
  return '程序会在此地址后追加 /chat/completions；也会尝试追加 /models 同步模型。'
})

function changeProvider() {
  models.value = []
  modelCatalogUpdatedAt.value = ''
  modelPickerActive.value = false
  modelQuery.value = ''
  if (settingsForm.llm_provider === 'openai' && (!settingsForm.llm_model || settingsForm.llm_model.startsWith('openai/'))) {
    settingsForm.llm_model = 'gpt-4o-mini'
  } else if (settingsForm.llm_provider === 'openrouter' && (!settingsForm.llm_model || settingsForm.llm_model === 'gpt-4o-mini')) {
    settingsForm.llm_model = 'openai/gpt-4o-mini'
  } else if (settingsForm.llm_provider === 'custom' && (settingsForm.llm_model === 'gpt-4o-mini' || settingsForm.llm_model === 'openai/gpt-4o-mini')) {
    settingsForm.llm_model = ''
  }
  settingsMessageKind.value = 'info'
  settingsMessage.value = `已切换为${providerLabel.value}，保存时请输入该服务商的 API Key。`
}

async function saveSettings(): Promise<boolean> {
  settingsBusy.value = true
  settingsMessageKind.value = 'info'
  settingsMessage.value = ''
  try {
    if (settingsForm.llm_provider !== savedProvider.value && !secretForm.llm_api_key.trim()) {
      settingsMessageKind.value = 'error'
      settingsMessage.value = '切换模型服务商时必须输入新的 API Key'
      return false
    }
    const values: Record<string, string> = {
      sui_book_name: settingsForm.sui_book_name,
      cleanup_retention_days: settingsForm.cleanup_retention_days,
      llm_provider: settingsForm.llm_provider,
      llm_base_url: settingsForm.llm_base_url,
      llm_model: settingsForm.llm_model,
      llm_prompt: settingsForm.llm_prompt,
    }
    if (secretForm.llm_api_key.trim()) values.llm_api_key = secretForm.llm_api_key.trim()
    const result = await api.updateSettings(values)
    Object.assign(settingsForm, {
      sui_book_name: result.settings.sui_book_name,
      cleanup_retention_days: result.settings.cleanup_retention_days,
      llm_provider: result.settings.llm_provider,
      llm_base_url: result.settings.llm_base_url,
      llm_model: result.settings.llm_model,
      llm_prompt: result.settings.llm_prompt,
    })
    savedProvider.value = result.settings.llm_provider
    secretStatus.value = result.secrets
    secretForm.llm_api_key = ''
    modelPickerActive.value = false
    modelQuery.value = ''
    settingsMessage.value = '配置已保存；敏感字段不会回显。'
    return true
  } catch (error) { settingsMessage.value = error instanceof Error ? error.message : '保存配置失败'; return false } finally { settingsBusy.value = false }
}

async function cleanupEntries() {
  if (cleanupBusy.value || settingsBusy.value) return
  cleanupBusy.value = true
  settingsMessageKind.value = 'info'
  settingsMessage.value = ''
  try {
    const result = await api.cleanupEntries()
    settingsMessageKind.value = 'success'
    const policy = result.retentionDays === 0 ? '永久保留' : `超过 ${result.retentionDays} 天`
    settingsMessage.value = `已按“${policy}”清理 ${result.deleted} 条已入账记录，并完成 SQLite VACUUM。`
  } catch (error) {
    settingsMessageKind.value = 'error'
    settingsMessage.value = error instanceof Error ? error.message : '手动清理失败'
  } finally { cleanupBusy.value = false }
}

async function refreshModels() {
  if (!await saveSettings()) return
  settingsBusy.value = true
  settingsMessageKind.value = 'info'
  settingsMessage.value = ''
  try {
    const result = await api.syncLLMModels()
    models.value = result.models
    modelCatalogUpdatedAt.value = result.fetchedAt
    settingsMessage.value = `已同步 ${result.models.length} 个${providerLabel.value}模型并保存到本地。`
  } catch (error) { settingsMessage.value = error instanceof Error ? error.message : '同步模型列表失败' } finally { settingsBusy.value = false }
}

async function toggleBasePrompt() {
  if (basePromptOpen.value) {
    basePromptOpen.value = false
    return
  }
  basePromptBusy.value = true
  basePromptMessage.value = ''
  try {
    const result = await api.llmPrompt()
    basePrompt.value = result.prompt
    basePromptOpen.value = true
  } catch (error) {
    basePromptMessage.value = error instanceof Error ? error.message : '读取基础 prompt 失败'
  } finally { basePromptBusy.value = false }
}

function toggleSettingsMetadataExpanded() {
  settingsMetadataExpanded.value = !settingsMetadataExpanded.value
}

function togglePageMetadataExpanded() {
  pageMetadataExpanded.value = !pageMetadataExpanded.value
}

function chooseModel(model: ModelOption) {
  settingsForm.llm_model = model.id
  modelPickerActive.value = false
  modelQuery.value = ''
  settingsMessageKind.value = 'info'
  settingsMessage.value = `已选择模型：${model.name || model.id}`
}

function beginModelSearch() {
  if (!modelPickerActive.value) {
    modelPickerActive.value = true
    modelQuery.value = settingsForm.llm_model
  }
}

function closeModelSearch() {
  window.setTimeout(() => {
    if (modelQuery.value.trim()) settingsForm.llm_model = modelQuery.value.trim()
    modelPickerActive.value = false
    modelQuery.value = ''
  }, 150)
}

async function syncBooks() {
  if (!await saveSettings()) {
    notifySync('error', settingsMessage.value || '同步账簿失败')
    return
  }
  settingsBusy.value = true
  settingsMessage.value = ''
  try {
    const result = await api.syncBooks()
    books.value = result.books
    booksUpdatedAt.value = result.updatedAt ?? ''
    const text = `已同步 ${result.books.length} 个账簿并保存到本地；当前会话账簿：${result.currentBookName}。分类尚未自动同步，请选择账簿后手动点击“同步当前账簿分类”。`
    notifySync('success', text)
  } catch (error) {
    const text = error instanceof Error ? error.message : '同步账簿失败'
    notifySync('error', text)
  } finally { settingsBusy.value = false }
}

function selectBook() {
  syncedMetadata.value = null
  basePromptOpen.value = false
  basePrompt.value = ''
  settingsMessageKind.value = 'info'
  settingsMessage.value = settingsForm.sui_book_name ? `已选择“${settingsForm.sui_book_name}”，请点击“同步当前账簿分类”。` : ''
}

async function syncSelectedBook() {
  if (!settingsForm.sui_book_name) return
  if (!await saveSettings()) {
    notifySync('error', settingsMessage.value || '同步分类失败')
    return
  }
  settingsBusy.value = true
  settingsMessage.value = ''
  try {
    const result = await api.syncBook(settingsForm.sui_book_name)
    syncedMetadata.value = result.metadata
    basePromptOpen.value = false
    basePrompt.value = ''
    const text = `已同步“${settingsForm.sui_book_name}”的分类：支出 ${result.metadata.categories.expense.length} 个，收入 ${result.metadata.categories.income.length} 个。`
    notifySync('success', text)
  } catch (error) {
    const text = error instanceof Error ? error.message : '同步分类失败'
    notifySync('error', text)
  } finally { settingsBusy.value = false }
}

async function loadMetadata() {
  const result = await api.getMetadata()
  if (result.metadata && settingsForm.sui_book_name.trim() && result.metadata.currentBook.name.trim() === settingsForm.sui_book_name.trim()) {
    syncedMetadata.value = result.metadata
  }
}

function isMobileViewport(): boolean {
  return window.innerWidth <= 760 || document.documentElement.clientWidth <= 760 || /Android|iPhone|iPad|iPod|Mobile/i.test(navigator.userAgent)
}

async function copyTextToClipboard(value: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value)
      return true
    } catch {
      // Fall back for browsers or contexts where the Clipboard API is unavailable.
    }
  }

  const textarea = document.createElement('textarea')
  textarea.value = value
  textarea.setAttribute('readonly', '')
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  let copied = false
  try {
    copied = document.execCommand('copy')
  } catch {
    copied = false
  }
  textarea.remove()
  return copied
}

async function submit() {
  const inputText = text.value.trim()
  if (!inputText || busy.value) return
  busy.value = true
  message.value = ''
  try {
    const target = editingEntry.value
    const result = target ? await api.reparse(target, inputText) : await api.ingest(inputText)
    text.value = ''
    editingEntry.value = null
    if (target || !isMobileViewport()) openEntry(result.entry)
    else closeEntryDetail()
    const successMessage = target ? `记录 #${target.id} 已提交重新解析，当前为待解析状态` : '记录已提交，当前为待解析状态；后台会自动解析并记账'
    try {
      await refresh()
      message.value = `${successMessage}。记录列表已刷新。`
    } catch {
    message.value = `${successMessage}，但记录列表刷新失败，页面会继续自动重试。`
    }
  } catch (error) { message.value = error instanceof Error ? error.message : '提交失败' } finally { busy.value = false }
}

async function editEntry(entry: Entry) {
  if (!canEditEntryState(entry.state)) {
    const copied = await copyTextToClipboard(entry.inputText)
    message.value = copied ? '当前流程不可覆盖；已将草稿复制到剪贴板。' : '当前流程不可覆盖；复制草稿失败，请手动新建一笔。'
    return
  }
  editingEntry.value = entry
  text.value = entry.inputText
  message.value = `已载入记录 #${entry.id}，修改后点击“提交”将重新进入后台解析。`
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

function exitEdit() {
  editingEntry.value = null
  text.value = ''
  message.value = ''
}

function openEntry(entry: Entry) {
  selected.value = entry
  submitPreviewData.value = null
  if (isBackgroundState(entry.state) && entry.state !== 'committing') {
    draft.value = null
    return
  }
  try {
    const parsed = JSON.parse(entry.draftJson) as Partial<Draft>
    draft.value = normalizeDraftForEditor(parsed)
  } catch { draft.value = emptyDraft() }
}

function closeEntryDetail() {
  submitFeedbackState.value = 'idle'
  submitFeedbackError.value = ''
  selected.value = null
  draft.value = null
  submitPreviewData.value = null
  submitPreviewResult.value = null
}

function emptyDraft(): Draft {
  return { type: 'expense', date: '', amount: '', categoryName: '', accountName: '', storeName: '', projectName: '', memberName: '', memo: '', confidence: 0, needsConfirmation: true }
}

function normalizeDraftForEditor(parsed: Partial<Draft>): Draft {
  const fallback = emptyDraft()
  const normalized: Draft = {
    type: parsed.type === 'income' ? 'income' : fallback.type,
    date: typeof parsed.date === 'string' ? parsed.date : fallback.date,
    amount: typeof parsed.amount === 'string' ? parsed.amount : fallback.amount,
    categoryName: typeof parsed.categoryName === 'string' ? parsed.categoryName : fallback.categoryName,
    accountName: typeof parsed.accountName === 'string' ? parsed.accountName : fallback.accountName,
    storeName: typeof parsed.storeName === 'string' ? parsed.storeName : fallback.storeName,
    projectName: typeof parsed.projectName === 'string' ? parsed.projectName : fallback.projectName,
    memberName: typeof parsed.memberName === 'string' ? parsed.memberName : fallback.memberName,
    memo: typeof parsed.memo === 'string' ? parsed.memo : fallback.memo,
    confidence: typeof parsed.confidence === 'number' ? parsed.confidence : fallback.confidence,
    needsConfirmation: parsed.needsConfirmation === true,
  }
  if (!normalized.accountName && syncedMetadata.value?.accounts[0]) normalized.accountName = syncedMetadata.value.accounts[0].name
  return normalized
}

function wait(milliseconds: number): Promise<void> {
  return new Promise(resolve => window.setTimeout(resolve, milliseconds))
}

async function saveDraft() {
  if (!selected.value || !draft.value) return
  busy.value = true
  try {
    const result = await api.update(selected.value, draft.value)
    closeEntryDetail()
    try {
      await refresh()
      message.value = `记录 #${result.entry.id} 的草稿已保存`
    } catch {
      message.value = `记录 #${result.entry.id} 的草稿已保存，但记录列表刷新失败，页面会继续自动重试。`
    }
  } catch (error) { message.value = error instanceof Error ? error.message : '保存失败' } finally { busy.value = false }
}

async function saveAndConfirm() {
  if (!selected.value || !draft.value || busy.value) return
  if (debugEnabled.value) {
    message.value = 'Debug 模式下请先保存草稿并点击“提交预览”，确认参数后再继续。'
    return
  }
  if (!requiredFieldsReady.value) {
    message.value = '处理前必须填写时间、金额、分类和账户；其他字段可以留空。'
    return
  }
  busy.value = true
  submitFeedbackError.value = ''
  submitFeedbackState.value = 'processing'
  detailClosing.value = false
  detailElement.value?.scrollTo({ top: 0 })
  try {
    const saved = await api.update(selected.value, draft.value)
    openEntry(saved.entry)
    const result = await api.confirm(saved.entry)
    openEntry(result.entry)
    if (result.entry.state !== 'committed') {
      submitFeedbackError.value = result.entry.state === 'uncertain'
        ? '提交结果不确定，请先核对随手记流水，不要自动重试。'
        : result.entry.errorMessage || '随手记拒绝了提交，已保存为待处理。'
      submitFeedbackState.value = 'error'
      try { await refresh() } catch { /* The entry state is already visible locally. */ }
      return
    }
    try { await refresh() } catch { /* A successful submit should not be shown as a failed submit. */ }
    submitFeedbackState.value = 'success'
    await wait(900)
    detailClosing.value = true
    await wait(260)
    closeEntryDetail()
    detailClosing.value = false
    submitFeedbackState.value = 'idle'
    message.value = `已入账${result.result?.id ? `（随手记记录 ${result.result.id}）` : ''}`
  } catch (error) {
    submitFeedbackState.value = 'error'
    detailClosing.value = false
    submitFeedbackError.value = error instanceof Error ? error.message : '保存并提交失败'
  } finally { busy.value = false }
}

async function reparse() {
  if (!selected.value) return
  busy.value = true
  message.value = ''
  try {
    const result = await api.reparse(selected.value)
    openEntry(result.entry)
    await refresh()
    message.value = '已按当前账簿资料和解析提示重新生成草稿，请核对后确认。'
  } catch (error) { message.value = error instanceof Error ? error.message : '重新解析失败' } finally { busy.value = false }
}

async function interruptProcessing() {
  if (!selected.value || selectedState.value !== 'processing' || busy.value) return
  interruptConfirmOpen.value = true
}

function closeInterruptConfirm() {
  if (!busy.value) interruptConfirmOpen.value = false
}

async function confirmInterruptProcessing() {
  if (!selected.value || selectedState.value !== 'processing' || busy.value) return
  interruptConfirmOpen.value = false
  busy.value = true
  message.value = ''
  try {
    const result = await api.interrupt(selected.value)
    openEntry(result.entry)
    await refresh()
    message.value = `记录 #${result.entry.id} 已手动中断解析，可重新解析。`
  } catch (error) { message.value = error instanceof Error ? error.message : '中断解析失败' } finally { busy.value = false }
}

async function previewSubmit() {
  if (!selected.value || !debugEnabled.value) return
  submitPreviewBusy.value = true
  message.value = ''
  try {
    submitPreviewData.value = await api.submitPreview(selected.value)
    submitPreviewHeaders.value = JSON.stringify(submitPreviewData.value.headers, null, 2)
    submitPreviewResult.value = null
    message.value = '提交预览已生成；当前为 debug 模式，尚未提交。'
  } catch (error) { message.value = error instanceof Error ? error.message : '生成提交预览失败' } finally { submitPreviewBusy.value = false }
}

function closeSubmitPreview() {
  submitPreviewData.value = null
  submitPreviewResult.value = null
  submitPreviewHeaders.value = ''
}

async function executeSubmitPreview() {
  if (!selected.value || !submitPreviewData.value || !debugEnabled.value) return
  let headers: Record<string, string>
  try {
    const parsed: unknown = JSON.parse(submitPreviewHeaders.value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('headers 必须是 JSON 对象')
    headers = Object.fromEntries(Object.entries(parsed).map(([key, value]) => [key, String(value)]))
  } catch (error) {
    message.value = error instanceof Error ? error.message : 'headers 格式不正确'
    return
  }
  submitPreviewBusy.value = true
  message.value = ''
  try {
    submitPreviewResult.value = await api.executeSubmitPreview(selected.value, { ...submitPreviewData.value, headers })
    message.value = submitPreviewResult.value.statusCode >= 200 && submitPreviewResult.value.statusCode < 300 ? '随手记已返回成功响应。' : '随手记返回失败响应，详情见提交预览。'
  } catch (error) { message.value = error instanceof Error ? error.message : '提交请求失败' } finally { submitPreviewBusy.value = false }
}

function resetCategoryForType() {
  if (draft.value) draft.value.categoryName = ''
}

function deleteEntry() {
  if (!selected.value || busy.value) return
  deleteConfirmOpen.value = true
}

function closeDeleteConfirm() {
  if (!busy.value) deleteConfirmOpen.value = false
}

async function confirmDeleteEntry() {
  if (!selected.value || busy.value) return
  deleteConfirmOpen.value = false
  busy.value = true
  try {
    await api.delete(selected.value)
    selected.value = null
    draft.value = null
    editingEntry.value = null
    text.value = ''
    await refresh()
    message.value = '记录已删除'
  } catch (error) { message.value = error instanceof Error ? error.message : '删除失败' } finally { busy.value = false }
}

onMounted(async () => {
  try {
    await Promise.all([refresh(), loadSettings(), loadBooks(), loadModels(), loadDebugStatus()])
    await loadMetadata()
    startRefreshTimer()
  } catch (error) { message.value = error instanceof Error ? error.message : '加载失败' }
})

onUnmounted(() => {
  stopRefreshTimer()
})
</script>

<template>
  <div class="shell">
    <header><div><p class="eyebrow">SUI ACCOUNTING BRIDGE</p><h1>随手记桥接器</h1></div><div class="header-actions"><span class="pill">单用户</span><button class="secondary config-trigger" @click="settingsOpen = true">配置</button></div></header>
    <Teleport to="body">
      <div v-if="settingsOpen" class="config-overlay">
        <section class="card config-dialog" role="dialog" aria-modal="true" aria-labelledby="config-title">
          <div class="section-title"><div><h2 id="config-title">配置</h2><p class="hint">普通配置可读取；API Key 只允许写入，不会从服务端回显。</p></div><button type="button" class="close-action" aria-label="关闭配置" title="关闭" @click="settingsOpen = false"><svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M6 6l12 12M18 6L6 18" /></svg></button></div>
        <div class="form-grid">
          <label class="form-field">目标账簿<select v-model="settingsForm.sui_book_name" :disabled="!books.length" @change="selectBook">
            <option value="" disabled>{{ books.length ? '请选择账簿' : '请先同步账簿' }}</option>
            <option v-for="(book, index) in books" :key="`${book.name}-${index}`" :value="book.name">{{ book.name }}{{ book.current ? '（当前）' : '' }}</option>
          </select></label>
          <label class="form-field">已入账记录保留时限<select v-model="settingsForm.cleanup_retention_days" :disabled="settingsBusy || cleanupBusy">
            <option v-for="option in cleanupRetentionOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
          </select><span class="field-hint">后台每天自动清理超期的已入账记录；待人工处理的记录不会被清理。永久只关闭自动删除。</span></label>
          <div class="form-field cleanup-field"><span class="field-label">本地记录清理</span><div class="cleanup-action-row"><button type="button" class="secondary compact-action" :disabled="settingsBusy || cleanupBusy" @click="cleanupEntries">{{ cleanupBusy ? '清理中…' : '立即清理已过期记录' }}</button><span class="field-hint">按已保存的保留时限执行，并回收 SQLite 空间。</span></div></div>
          <label class="form-field">AI 服务商<select v-model="settingsForm.llm_provider" @change="changeProvider">
            <option value="openai">OpenAI</option>
            <option value="openrouter">OpenRouter</option>
            <option value="custom">自定义（OpenAI-compatible）</option>
          </select></label>
          <label class="form-field">API Key<input v-model="secretForm.llm_api_key" type="password" autocomplete="new-password" placeholder="留空保持不变；切换服务商时必须输入"><span class="field-hint">{{ providerLabel }} 当前状态：{{ secretStatus.llm_api_key ? '已配置' : '未配置' }}</span></label>
          <label v-if="customProvider" class="form-field">API Base URL<input v-model="settingsForm.llm_base_url" type="url" placeholder="例如：https://example.com/v1"><span class="field-hint">{{ providerBaseURLHint }}</span></label>
          <div v-else class="form-field"><span class="field-label">API Base URL</span><span class="field-hint">{{ providerBaseURLHint }}</span></div>
          <div class="model-picker full-width">
            <div class="field-label-row">
              <label class="field-label" for="llm-model">模型</label>
              <button type="button" class="link model-sync-link" :disabled="settingsBusy" @click="refreshModels">同步模型列表</button>
            </div>
            <div class="model-input-wrap">
              <input id="llm-model" v-model="modelInput" type="search" :class="{ 'model-input-with-hint': models.length && settingsForm.llm_model && !modelPickerActive }" @focus="beginModelSearch" @blur="closeModelSearch" placeholder="可直接输入模型 ID，或同步后搜索">
              <span v-if="models.length && settingsForm.llm_model && !modelPickerActive" class="model-input-hint" aria-hidden="true">点击输入框可重新搜索</span>
              <div v-if="modelPickerActive && modelMatches.length" class="model-hints">
                <button v-for="model in modelMatches" :key="model.id" type="button" class="model-hint" @mousedown.prevent @click="chooseModel(model)">
                  <span>{{ model.name || model.id }}</span><small>{{ model.id }}</small>
                </button>
              </div>
            </div>
            <span v-if="models.length" class="field-hint">已保存模型列表，共 {{ models.length }} 个模型{{ modelCatalogUpdatedAt ? `；同步于 ${formatBrowserDateTime(modelCatalogUpdatedAt)}` : '' }}</span>
            <span v-else class="field-hint">模型列表同步是可选的；自定义端点不支持 /models 时可以直接填写模型 ID。</span>
            <p v-if="modelPickerActive && modelQuery && models.length && !modelMatches.length" class="field-hint">没有匹配的模型。</p>
          </div>
          <label class="prompt-editor full-width">
            <div class="field-label-row"><span class="field-label">AI 解析补充 prompt</span><button type="button" class="link model-sync-link" :disabled="basePromptBusy" @click="toggleBasePrompt">{{ basePromptOpen ? '收起基础 prompt' : '预览基础 prompt' }}</button></div>
            <textarea v-model="settingsForm.llm_prompt" rows="5" placeholder="例如：午餐优先归入食品；备注保留原始地点和同行人。"></textarea>
            <span class="field-hint">这里只编辑补充 prompt。基础 prompt 由系统生成，包含当前账簿分类和 JSON 输出规则，不能编辑。</span>
            <div v-if="basePromptOpen" class="prompt-preview"><div class="prompt-preview-title">基础 prompt · 只读</div><pre>{{ basePrompt }}</pre></div>
            <p v-if="basePromptMessage" class="field-hint prompt-preview-error">{{ basePromptMessage }}</p>
          </label>
        </div>
        <div class="actions config-actions"><button class="secondary compact-action" :disabled="settingsBusy" @click="syncBooks">同步随手记账本列表</button><button class="secondary compact-action" :disabled="settingsBusy || !books.length || !settingsForm.sui_book_name" @click="syncSelectedBook">同步当前账簿分类</button><button class="save-config" :disabled="settingsBusy" @click="saveSettings">保存配置</button></div>
        <div v-if="syncedMetadata" class="synced-data">
          <div class="section-title metadata-section-title"><div class="metadata-heading"><button type="button" class="metadata-disclosure" :aria-label="settingsMetadataExpanded ? '收起账本资料' : '展开账本资料'" :aria-expanded="settingsMetadataExpanded" @click="toggleSettingsMetadataExpanded"><span class="toggle-chevron" :class="{ expanded: settingsMetadataExpanded }" aria-hidden="true"></span></button><div><h3>已同步账本资料</h3><div class="metadata-sync-info"><span class="field-hint">共 {{ books.length }} 个账本 · 账本资料同步于 <Transition name="time-fade" mode="out-in"><span :key="syncedMetadata.fetchedAt" class="synced-time">{{ formatBrowserDateTime(syncedMetadata.fetchedAt) }}</span></Transition></span><span class="field-hint">当前账簿：{{ syncedMetadata.currentBook.name }}<template v-if="booksUpdatedAt"> · 账本列表同步于 <Transition name="time-fade" mode="out-in"><span :key="booksUpdatedAt" class="synced-time">{{ formatBrowserDateTime(booksUpdatedAt) }}</span></Transition></template></span></div></div></div></div>
          <template v-if="settingsMetadataExpanded">
          <div class="category-columns">
            <div><h4>支出分类（{{ syncedMetadata.categories.expense.length }}）</h4><p class="field-hint">一级分类仅用于分组，记账必须选择二级分类。</p><div class="category-tree"><div v-for="group in expenseCategoryGroups" :key="`expense-group-${group.parent.id}`" class="category-group"><div class="category-parent" :class="{ 'category-leaf': isLeafCategory(group.parent) }"><span>{{ group.parent.name }}</span><small>{{ isLeafCategory(group.parent) ? '二级分类' : '一级分类（不可直接记账）' }}</small></div><ul v-if="group.children.length" class="category-children"><li v-for="item in group.children" :key="`expense-${item.id}`" class="category-child"><span>↳ {{ item.name }}</span><small>二级分类</small></li></ul></div></div></div>
            <div><h4>收入分类（{{ syncedMetadata.categories.income.length }}）</h4><p class="field-hint">一级分类仅用于分组，记账必须选择二级分类。</p><div class="category-tree"><div v-for="group in incomeCategoryGroups" :key="`income-group-${group.parent.id}`" class="category-group"><div class="category-parent" :class="{ 'category-leaf': isLeafCategory(group.parent) }"><span>{{ group.parent.name }}</span><small>{{ isLeafCategory(group.parent) ? '二级分类' : '一级分类（不可直接记账）' }}</small></div><ul v-if="group.children.length" class="category-children"><li v-for="item in group.children" :key="`income-${item.id}`" class="category-child"><span>↳ {{ item.name }}</span><small>二级分类</small></li></ul></div></div></div>
          </div>
          <div class="metadata-extra-columns"><div><h4>商家（{{ syncedMetadata.stores.length }}）</h4><ul><li v-for="item in syncedMetadata.stores" :key="`store-${item.id}`">{{ item.name }}</li></ul></div><div><h4>账户（{{ syncedMetadata.accounts.length }}）</h4><ul><li v-for="item in syncedMetadata.accounts" :key="`account-${item.id}`">{{ item.name }}</li></ul></div><div><h4>项目（{{ syncedMetadata.projects.length }}）</h4><ul><li v-for="item in syncedMetadata.projects" :key="`project-${item.id}`">{{ item.name }}</li></ul></div><div><h4>成员（{{ syncedMetadata.members.length }}）</h4><ul><li v-for="item in syncedMetadata.members" :key="`member-${item.id}`">{{ item.name }}</li></ul></div></div>
          </template>
        </div>
        <p v-if="settingsMessage" class="notice" :class="`notice-${settingsMessageKind}`" role="status">{{ settingsMessage }}</p>
        </section>
      </div>
    </Teleport>
    <Teleport to="body">
      <div v-if="debugEnabled && submitPreviewData" class="submit-preview-overlay" @click.self="closeSubmitPreview">
        <section class="card submit-preview-dialog" role="dialog" aria-modal="true" aria-labelledby="submit-preview-title">
          <div class="section-title"><div><h2 id="submit-preview-title">提交预览 · Debug</h2><p class="hint">仅预览不会自动提交；可修改请求参数后点击“直接提交”。</p></div><button class="link" @click="closeSubmitPreview">关闭</button></div>
          <label>Method<input v-model="submitPreviewData.method" :disabled="submitPreviewBusy"></label>
          <label>URL<input v-model="submitPreviewData.url" :disabled="submitPreviewBusy"></label>
          <label>Headers<textarea v-model="submitPreviewHeaders" rows="5" :disabled="submitPreviewBusy"></textarea><span class="field-hint">JSON 对象；Cookie/Authorization 由后台保护，不接受前台覆盖。</span></label>
          <label>Payload<textarea v-model="submitPreviewData.payload" rows="8" :disabled="submitPreviewBusy"></textarea></label>
          <div class="actions submit-preview-actions"><button :disabled="submitPreviewBusy" @click="executeSubmitPreview">{{ submitPreviewBusy ? '提交中…' : '直接提交' }}</button><button class="secondary" :disabled="submitPreviewBusy" @click="closeSubmitPreview">关闭</button></div>
          <div v-if="submitPreviewResult" class="submit-response"><div class="submit-preview-title">随手记返回</div><pre>{{ submitPreviewResultText }}</pre></div>
        </section>
      </div>
    </Teleport>
    <Teleport to="body">
      <div v-if="interruptConfirmOpen" class="confirm-overlay" @click.self="closeInterruptConfirm">
        <section class="card confirm-dialog" role="dialog" aria-modal="true" aria-labelledby="interrupt-confirm-title">
          <div class="section-title"><div><h2 id="interrupt-confirm-title">中断解析</h2><p class="hint">只会中断当前记录，不影响其他记录排队或解析。</p></div><button class="link" :disabled="busy" @click="closeInterruptConfirm">关闭</button></div>
          <p class="confirm-message">确定中断记录 #{{ selected?.id }} 的解析吗？中断后可以重新解析。</p>
          <div class="actions confirm-actions"><button class="secondary" :disabled="busy" @click="closeInterruptConfirm">取消</button><button class="danger-action" :disabled="busy" @click="confirmInterruptProcessing">{{ busy ? '中断中…' : '中断解析' }}</button></div>
        </section>
      </div>
    </Teleport>
    <Teleport to="body">
      <div v-if="deleteConfirmOpen" class="confirm-overlay" @click.self="closeDeleteConfirm">
        <section class="card confirm-dialog" role="dialog" aria-modal="true" aria-labelledby="delete-confirm-title">
          <div class="section-title"><div><h2 id="delete-confirm-title">删除记录</h2><p class="hint">删除后不可恢复。</p></div><button class="link" :disabled="busy" @click="closeDeleteConfirm">关闭</button></div>
          <p class="confirm-message">确定删除记录 #{{ selected?.id }}？</p>
          <div class="actions confirm-actions"><button class="secondary" :disabled="busy" @click="closeDeleteConfirm">取消</button><button class="danger-action" :disabled="busy" @click="confirmDeleteEntry">删除记录</button></div>
        </section>
      </div>
    </Teleport>
    <section class="card composer"><label for="input">{{ editing ? '修改一笔' : '记一笔' }}</label><textarea id="input" v-model="text" placeholder="例如：今天中午在餐馆吃饭 35 元"></textarea><div class="actions"><span class="hint">{{ editing ? `正在修改记录 #${editingEntry?.id}；提交后会重新进入后台解析` : ledgerReady ? `点击提交后先保存为待解析，后台按“${syncedMetadata?.currentBook.name}”的账本资料处理` : '点击提交后会先保存为待解析；后台解析需要当前账簿分类资料' }}</span><button v-if="editing" class="secondary" :disabled="busy" @click="exitEdit">退出修改</button><button :disabled="busy || !text.trim()" @click="submit">{{ busy ? '提交中…' : '提交' }}</button></div></section>
    <p v-if="message" class="notice" role="status" aria-live="polite">{{ message }}</p>
    <div class="layout"><section class="card"><div class="section-title record-section-title"><div class="record-heading"><h2>记录</h2><button type="button" class="icon-action reconcile-button" :disabled="refreshBusy" aria-label="刷新并核对" title="刷新并核对" @click="refreshAndReconcile"><svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M3 12a9 9 0 0 1 15-6.7L21 8" /><path d="M21 3v5h-5" /><path d="M21 12a9 9 0 0 1-15 6.7L3 16" /><path d="M3 21v-5h5" /></svg></button><p class="field-hint">右键记录可修改原草稿</p></div><div class="entry-list-actions"><div class="record-filter" role="group" aria-label="记录筛选"><button type="button" :class="{ active: !manualOnly }" :aria-pressed="!manualOnly" @click="manualOnly = false">全部</button><button type="button" :class="{ active: manualOnly }" :aria-pressed="manualOnly" @click="manualOnly = true">待处理</button></div></div></div><p v-if="!visibleEntries.length" class="empty">{{ manualOnly ? '没有需要手动确认的记录。' : '还没有记录。' }}</p><button v-for="entry in visibleEntries" :key="entry.id" class="entry-row" :class="{ active: selected?.id === entry.id }" :disabled="busy" @click="submitFeedbackState = 'idle'; openEntry(entry)" @contextmenu.prevent="!busy && editEntry(entry)"><span><strong>#{{ entry.id }}</strong> {{ entry.inputText }}</span><span class="state" :class="stateClass(entry.state)">{{ entryStateLabel(entry) }}</span></button></section>
      <section ref="detailElement" class="card detail" :class="{ 'detail-closing': detailClosing, 'detail-feedback-active': submitFeedbackState !== 'idle' }"><template v-if="selected && (selectedState === 'queued' || selectedState === 'processing')"><div class="section-title"><h2>记录 #{{ selected.id }}</h2><span class="state" :class="stateClass(selectedState)">{{ selectedStateLabel }}</span></div><div class="processing-panel"><p>{{ selectedState === 'queued' ? '记录已保存，等待后台解析。' : '后台正在解析这条记录，请稍候。' }}</p><p class="field-hint">解析完成后，字段明确的记录会自动提交；只有需要人工判断的记录才会停留在待人工确认。</p></div><div v-if="selectedState === 'processing'" class="actions"><button class="danger-action" :disabled="busy" @click="interruptProcessing">{{ busy ? '中断中…' : '中断解析' }}</button></div></template><template v-else-if="selected && draft"><div class="section-title"><h2>记录 #{{ selected.id }}</h2><span class="state" :class="stateClass(selectedState)">{{ selectedStateLabel }}</span></div><label :inert="submitFeedbackState !== 'idle'">类型<select v-model="draft.type" @change="resetCategoryForType"><option value="expense">支出</option><option value="income">收入</option></select></label><label :inert="submitFeedbackState !== 'idle'">日期<input v-model="draft.date"></label><label :inert="submitFeedbackState !== 'idle'">账户<select v-model="draft.accountName" :disabled="!syncedMetadata?.accounts.length"><option value="" disabled>{{ syncedMetadata?.accounts.length ? '请选择账户' : '请先同步当前账簿资料' }}</option><option v-for="item in syncedMetadata?.accounts ?? []" :key="`draft-account-${item.id}`" :value="item.name">{{ item.name }}</option></select></label><label :inert="submitFeedbackState !== 'idle'">金额<input v-model="draft.amount" type="text" inputmode="decimal" autocomplete="off"></label><label :inert="submitFeedbackState !== 'idle'">分类<select v-model="draft.categoryName" :disabled="!categoryOptions.length"><option value="" disabled>{{ categoryOptions.length ? '请选择二级分类' : '请先同步当前账簿分类' }}</option><option v-for="item in categoryOptions" :key="`draft-category-${item.id}`" :value="categoryLabel(item)">{{ categoryLabel(item) }}</option></select></label><label :inert="submitFeedbackState !== 'idle'">商家<select v-model="draft.storeName" :disabled="!syncedMetadata?.stores.length"><option value="">无</option><option v-for="item in syncedMetadata?.stores ?? []" :key="`draft-store-${item.id}`" :value="item.name">{{ item.name }}</option></select></label><label :inert="submitFeedbackState !== 'idle'">项目<select v-model="draft.projectName" :disabled="!syncedMetadata?.projects.length"><option value="">无</option><option v-for="item in syncedMetadata?.projects ?? []" :key="`draft-project-${item.id}`" :value="item.name">{{ item.name }}</option></select></label><label :inert="submitFeedbackState !== 'idle'">成员<select v-model="draft.memberName" :disabled="!syncedMetadata?.members.length"><option value="">无</option><option v-for="item in syncedMetadata?.members ?? []" :key="`draft-member-${item.id}`" :value="item.name">{{ item.name }}</option></select></label><label :inert="submitFeedbackState !== 'idle'">备注<textarea v-model="draft.memo"></textarea></label><div class="actions" :inert="submitFeedbackState !== 'idle'"><button v-if="debugEnabled" class="secondary" :disabled="busy || submitPreviewBusy || !requiredFieldsReady" @click="previewSubmit">{{ submitPreviewBusy ? '生成预览…' : '提交预览' }}</button><button class="secondary" :disabled="busy || !canEditEntryState(selectedState)" @click="reparse">重新解析</button><button :disabled="busy" @click="saveDraft">保存草稿</button><button :disabled="busy" @click="saveAndConfirm">保存并提交</button><div class="danger-zone"><button class="secondary" :disabled="busy" @click="deleteEntry">删除</button></div></div><div v-if="selected.errorMessage && ['rejected', 'uncertain', 'needs_confirmation'].includes(selectedState)" class="error-panel" role="alert"><strong>失败详情</strong><p>{{ selected.errorMessage }}</p><small v-if="selected.errorCode">错误代码：{{ selected.errorCode }}</small></div></template><p v-else class="empty">选择一条记录查看草稿。</p>
        <Transition name="submit-feedback-fade">
          <div v-if="submitFeedbackState !== 'idle'" class="submit-feedback-overlay" role="status" aria-live="polite" aria-atomic="true">
            <section class="submit-feedback-card" :class="`submit-feedback-state-${submitFeedbackState}`">
              <div v-if="submitFeedbackState === 'processing'" class="submit-feedback-spinner" aria-hidden="true"><span></span><span></span><span></span></div>
              <div v-else-if="submitFeedbackState === 'success'" class="submit-feedback-success-icon" aria-hidden="true"><svg viewBox="0 0 52 52" focusable="false"><path d="M14 27l8 8 17-19" /></svg></div>
              <div v-else class="submit-feedback-error-icon" aria-hidden="true"><svg viewBox="0 0 24 24" focusable="false"><path d="M6 6l12 12M18 6L6 18" /></svg></div>
              <strong>{{ submitFeedbackState === 'processing' ? '正在提交，请稍候' : submitFeedbackState === 'success' ? '提交成功' : selectedState === 'uncertain' ? '提交结果待核对' : '提交失败' }}</strong>
              <span class="field-hint">{{ submitFeedbackState === 'processing' ? '正在处理随手记记账请求。' : submitFeedbackState === 'success' ? '已成功记账，编辑窗口即将关闭。' : submitFeedbackError }}</span>
              <button v-if="submitFeedbackState === 'error'" type="button" class="secondary" @click="submitFeedbackState = 'idle'">返回编辑</button>
            </section>
          </div>
        </Transition>
      </section>
    </div>
    <section v-if="syncedMetadata" class="card metadata-card"><div class="section-title metadata-section-title"><div class="metadata-heading"><button type="button" class="metadata-disclosure" :aria-label="pageMetadataExpanded ? '收起账本资料' : '展开账本资料'" :aria-expanded="pageMetadataExpanded" @click="togglePageMetadataExpanded"><span class="toggle-chevron" :class="{ expanded: pageMetadataExpanded }" aria-hidden="true"></span></button><div><h2>账本资料</h2><div class="metadata-sync-info"><span class="field-hint">共 {{ books.length }} 个账本 · 账本资料同步于 <Transition name="time-fade" mode="out-in"><span :key="syncedMetadata.fetchedAt" class="synced-time">{{ formatBrowserDateTime(syncedMetadata.fetchedAt) }}</span></Transition></span><span class="field-hint">当前账簿：{{ syncedMetadata.currentBook.name }}<template v-if="booksUpdatedAt"> · 账本列表同步于 <Transition name="time-fade" mode="out-in"><span :key="booksUpdatedAt" class="synced-time">{{ formatBrowserDateTime(booksUpdatedAt) }}</span></Transition></template></span></div></div></div></div><template v-if="pageMetadataExpanded"><div class="category-columns"><div><h3>支出分类（{{ syncedMetadata.categories.expense.length }}）</h3><p class="field-hint">一级分类仅用于分组，记账必须选择二级分类。</p><div class="category-tree"><div v-for="group in expenseCategoryGroups" :key="`main-expense-group-${group.parent.id}`" class="category-group"><div class="category-parent" :class="{ 'category-leaf': isLeafCategory(group.parent) }"><span>{{ group.parent.name }}</span><small>{{ isLeafCategory(group.parent) ? '二级分类' : '一级分类（不可直接记账）' }}</small></div><ul v-if="group.children.length" class="category-children"><li v-for="item in group.children" :key="`main-expense-${item.id}`" class="category-child"><span>↳ {{ item.name }}</span><small>二级分类</small></li></ul></div></div></div><div><h3>收入分类（{{ syncedMetadata.categories.income.length }}）</h3><p class="field-hint">一级分类仅用于分组，记账必须选择二级分类。</p><div class="category-tree"><div v-for="group in incomeCategoryGroups" :key="`main-income-group-${group.parent.id}`" class="category-group"><div class="category-parent" :class="{ 'category-leaf': isLeafCategory(group.parent) }"><span>{{ group.parent.name }}</span><small>{{ isLeafCategory(group.parent) ? '二级分类' : '一级分类（不可直接记账）' }}</small></div><ul v-if="group.children.length" class="category-children"><li v-for="item in group.children" :key="`main-income-${item.id}`" class="category-child"><span>↳ {{ item.name }}</span><small>二级分类</small></li></ul></div></div></div></div><div class="metadata-extra-columns"><div><h4>商家（{{ syncedMetadata.stores.length }}）</h4><ul><li v-for="item in syncedMetadata.stores" :key="`main-store-${item.id}`">{{ item.name }}</li></ul></div><div><h4>账户（{{ syncedMetadata.accounts.length }}）</h4><ul><li v-for="item in syncedMetadata.accounts" :key="`main-account-${item.id}`">{{ item.name }}</li></ul></div><div><h4>项目（{{ syncedMetadata.projects.length }}）</h4><ul><li v-for="item in syncedMetadata.projects" :key="`main-project-${item.id}`">{{ item.name }}</li></ul></div><div><h4>成员（{{ syncedMetadata.members.length }}）</h4><ul><li v-for="item in syncedMetadata.members" :key="`main-member-${item.id}`">{{ item.name }}</li></ul></div></div><div class="metadata-actions"><button @click="settingsOpen = true">重新同步</button></div></template></section>
    <Teleport to="body">
      <button v-if="selected" type="button" class="mobile-detail-close" :disabled="busy" aria-label="关闭记录编辑器" @click="closeEntryDetail">关闭</button>
    </Teleport>
  </div>
</template>
