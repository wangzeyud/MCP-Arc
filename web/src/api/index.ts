import axios from 'axios'
import { ElMessage } from 'element-plus'

const token = localStorage.getItem('mcp_arc_token') || 'change-me'

export const api = axios.create({
  baseURL: '/api',
  headers: { Authorization: `Bearer ${token}` },
})

// A mismatched token makes every endpoint answer 401, which used to surface as a
// silently empty console (all-zero metrics, no error at all). Report it once,
// loudly, with the fix: it is the most common "the console shows nothing" cause.
let lastAuthNotice = 0
api.interceptors.response.use(
  (res) => res,
  (err) => {
    if (err?.response?.status === 401) {
      const now = Date.now()
      if (now - lastAuthNotice > 3000) {
        lastAuthNotice = now
        ElMessage.error(
          '鉴权失败 (401)：控制台 Token 与 mcp-arc 配置里的 admin.token 不一致，请点右上角 Token 修改。',
        )
      }
    }
    return Promise.reject(err)
  },
)

/** Human-readable message for a failed request. Yields '' for 401 because that
 *  case is already reported globally with an actionable hint. */
export function errorText(e: any): string {
  if (e?.response?.status === 401) return ''
  return String(e?.response?.data?.error || e?.message || e || 'request failed')
}

export interface CallRecord {
  id: number
  client_id: string
  tool_name: string
  params: string
  result: string
  error_msg: string
  latency_ms: number
  timestamp: string
  replay_of?: number
}

export interface Stats {
  total_calls: number
  error_count: number
  error_rate: number
  avg_latency_ms: number
  avg_latency_us: number
  latency_p50_us: number
  latency_p95_us: number
  latency_p99_us: number
  tool_counts: Record<string, number>
  client_counts?: Record<string, number>
  series?: { bucket: string; count: number }[]
}

export interface MaskRule {
  id: number
  name: string
  patterns: string[]
  fields: string[]
  mask_char: string
  enabled: boolean
  source: string
}

export type RuleInput = Partial<Omit<MaskRule, 'id' | 'source'>>

export async function fetchLogs(params: Record<string, any> = {}) {
  const { data } = await api.get('/logs', { params })
  return data.records as CallRecord[]
}

export async function fetchStats() {
  const { data } = await api.get('/stats')
  return data as Stats
}

export interface Status {
  client_transport: string
  sse_url: string
  console_url: string
  admin_port: number
  /** PID of the process serving this console — disambiguates side-by-side
   *  instances (and a stale tab) when several mcp-arc processes are running. */
  pid: number
  /** RFC3339 process start time. */
  started_at: string
  /** "sqlite" | "postgres" | "memory"; "memory" loses everything on exit. */
  audit_driver: string
  config_dir?: string
}

/** Effective addresses (console URL + the SSE endpoint to paste into the MCP client). */
export async function fetchStatus() {
  const { data } = await api.get('/status')
  return data as Status
}

export interface ReplayResult {
  tool: string
  response: any
  diff?: { mode: string; match: boolean }
  recorded?: any
}

export async function fetchReplay(callId: number, diff = false): Promise<ReplayResult> {
  const { data } = await api.post('/replay', { call_id: callId, diff })
  return data as ReplayResult
}

/** Pull the inner `result` (or `error`) out of a JSON-RPC response envelope so
 *  the console shows the meaningful payload rather than the full {jsonrpc,id,...}. */
export function extractPayload(envelope: any): any {
  if (envelope && typeof envelope === 'object') {
    if ('result' in envelope) return envelope.result
    if ('error' in envelope) return envelope.error
  }
  return envelope
}

export interface ReplayItem {
  call_id: number
  tool?: string
  status: string // ok | error | skipped
  response?: any
  error?: string
  diff?: { mode: string; match: boolean }
}

export interface ReplayBatchResult {
  results: ReplayItem[]
}

/** Batch replay: by explicit call_ids or a filter. `diff` compares each response
 *  against the originally recorded result (exact JSON equality). */
export async function fetchReplayBatch(opts: {
  call_ids?: number[]
  filter?: Record<string, any>
  diff?: boolean
}) {
  const { data } = await api.post('/replay/batch', opts)
  return data as ReplayBatchResult
}

export async function fetchRules() {
  const { data } = await api.get('/rules')
  return data.rules as MaskRule[]
}

export async function createRule(rule: RuleInput) {
  const { data } = await api.post('/rules', rule)
  return data as MaskRule
}

export async function updateRule(id: number, rule: RuleInput) {
  const { data } = await api.put(`/rules/${id}`, rule)
  return data as MaskRule
}

export async function deleteRule(id: number) {
  await api.delete(`/rules/${id}`)
}

/** Export the audit log. `raw=1` additionally includes unmasked params/results. */
export async function exportCalls(
  format: 'json' | 'csv',
  params: Record<string, any> = {},
): Promise<Blob> {
  const { data } = await api.get('/export', {
    params: { format, ...params },
    responseType: 'blob',
  })
  return data as Blob
}

/** Turn an exported blob into a browser download. */
export function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}
