export type EntryState = 'queued' | 'processing' | 'pending_submission' | 'needs_confirmation' | 'committing' | 'committed' | 'rejected' | 'uncertain'

export interface Draft {
  type: 'expense' | 'income'
  date: string
  amount: string
  categoryName: string
  accountName: string
  storeName: string
  projectName: string
  memberName: string
  memo: string
  confidence: number
  needsConfirmation: boolean
}

export interface Entry {
  id: number
  source: string
  clientRequestId: string
  inputType: string
  inputText: string
  state: EntryState
  version: number
  reconcileMisses?: number
  draftJson: string
  errorCode?: string
  errorMessage?: string
  createdAt: string
  updatedAt: string
}

export interface Settings {
  sui_book_name: string
  cleanup_retention_days: string
  llm_provider: 'openai' | 'openrouter' | 'custom'
  llm_base_url: string
  llm_model: string
  llm_prompt: string
  openrouter_model?: string
  openrouter_prompt?: string
}

export interface SecretStatus {
  llm_api_key: boolean
  openrouter_api_key?: boolean
}

export interface ModelOption {
  id: string
  name: string
  contextLength?: number
  supportedParameters?: string[]
}

export interface BookOption {
  name: string
  current: boolean
}

export interface CategoryOption {
  id: string
  name: string
  parentId?: string
  parentName?: string
  type?: string
  level?: number
  isLeaf?: boolean
}

export interface SyncedMetadata {
  currentBook: { id: string; name: string; isCurrent?: boolean }
  books: Array<{ id: string; name: string; isCurrent?: boolean }>
  categories: { expense: CategoryOption[]; income: CategoryOption[] }
  stores: CategoryOption[]
  accounts: CategoryOption[]
  projects: CategoryOption[]
  members: CategoryOption[]
  fetchedAt: string
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) }, ...init })
  const raw = await response.text()
  let body: unknown = null
  try { body = raw ? JSON.parse(raw) : null } catch { /* Keep the HTTP status for non-JSON server responses. */ }
  if (!response.ok) {
    const message = body && typeof body === 'object' && 'message' in body && typeof body.message === 'string' ? body.message : `请求失败 (${response.status})`
    throw new Error(message)
  }
  return body as T
}

export const api = {
  listEntries: () => request<{ entries: Entry[] }>('/api/v1/entries'),
  reconcileUncertain: () => request<{ status: string; checked: number; committed: number; needsConfirmation: number; errors: number }>('/api/v1/entries/reconcile-uncertain', { method: 'POST' }),
  ingest: (text: string) => request<{ entry: Entry }>('/api/v1/ingest', { method: 'POST', body: JSON.stringify({ text, inputType: 'text', source: 'web' }) }),
  update: (entry: Entry, draft: Draft) => request<{ entry: Entry }>(`/api/v1/entries/${entry.id}`, {
    method: 'PATCH',
    // Amounts are part of the wire contract as decimal strings. Keep that
    // contract even if a browser control hands us a numeric value.
    body: JSON.stringify({ version: entry.version, draft: { ...draft, amount: String(draft.amount ?? '') } }),
  }),
  reparse: (entry: Entry, text = entry.inputText) => request<{ entry: Entry }>(`/api/v1/entries/${entry.id}/reparse`, { method: 'POST', body: JSON.stringify({ text }) }),
  interrupt: (entry: Entry) => request<{ entry: Entry }>(`/api/v1/entries/${entry.id}/interrupt`, { method: 'POST' }),
  submitPreview: (entry: Entry) => request<SubmitPreview>(`/api/v1/entries/${entry.id}/submit-preview`, { method: 'POST' }),
  executeSubmitPreview: (entry: Entry, preview: SubmitPreview) => request<SubmitPreviewResult>(`/api/v1/entries/${entry.id}/submit-preview/execute`, { method: 'POST', body: JSON.stringify(preview) }),
  confirm: (entry: Entry) => request<{ entry: Entry; result?: { id?: string; budget?: string; price?: string } }>(`/api/v1/entries/${entry.id}/confirm`, { method: 'POST' }),
  delete: (entry: Entry) => request<{ status: string; id: number }>(`/api/v1/entries/${entry.id}`, { method: 'DELETE' }),
  getSettings: () => request<{ settings: Settings; secrets: SecretStatus }>('/api/v1/settings'),
  updateSettings: (values: Record<string, string>) => request<{ settings: Settings; secrets: SecretStatus }>('/api/v1/settings', { method: 'PATCH', body: JSON.stringify(values) }),
  cleanupEntries: () => request<{ status: string; deleted: number; retentionDays: number; vacuum: boolean }>('/api/v1/maintenance/cleanup', { method: 'POST' }),
  llmModels: () => request<{ status: string; models: ModelOption[]; fetchedAt: string }>('/api/v1/llm/models'),
  syncLLMModels: () => request<{ status: string; models: ModelOption[]; fetchedAt: string }>('/api/v1/llm/models/sync', { method: 'POST' }),
  llmPrompt: () => request<{ prompt: string; generatedAt: string }>('/api/v1/llm/prompt'),
  // Deprecated aliases retained for older callers.
  openRouterModels: () => request<{ status: string; models: ModelOption[]; fetchedAt: string }>('/api/v1/openrouter/models'),
  syncOpenRouterModels: () => request<{ status: string; models: ModelOption[]; fetchedAt: string }>('/api/v1/openrouter/models/sync', { method: 'POST' }),
  openRouterPrompt: () => request<{ prompt: string; generatedAt: string }>('/api/v1/openrouter/prompt'),
  debugStatus: () => request<{ enabled: boolean }>('/api/v1/debug'),
  listBooks: () => request<{ status: string; currentBookName: string; books: BookOption[]; updatedAt?: string }>('/api/v1/sui/books'),
  syncBooks: () => request<{ status: string; currentBookName: string; books: BookOption[]; updatedAt?: string }>('/api/v1/sui/books/sync', { method: 'POST' }),
  syncBook: (bookName: string) => request<{ status: string; metadata: SyncedMetadata }>('/api/v1/sui/sync', { method: 'POST', body: JSON.stringify({ bookName }) }),
  getMetadata: () => request<{ status: string; metadata: SyncedMetadata | null }>('/api/v1/metadata'),
}

export interface SubmitPreview {
  method: string
  url: string
  headers: Record<string, string>
  payload: string
}

export interface SubmitPreviewResult {
  statusCode: number
  status: string
  headers: Record<string, string>
  body: string
}
