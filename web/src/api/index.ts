import axios from 'axios'

const token = localStorage.getItem('mcp_arc_token') || 'change-me'

export const api = axios.create({
  baseURL: '/api',
  headers: { Authorization: `Bearer ${token}` },
})

export interface CallRecord {
  id: number
  client_id: string
  tool_name: string
  params: string
  result: string
  error_msg: string
  latency_ms: number
  timestamp: string
}

export interface Stats {
  total_calls: number
  error_count: number
  avg_latency_ms: number
  tool_counts: Record<string, number>
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
}

/** Effective addresses (console URL + the SSE endpoint to paste into the MCP client). */
export async function fetchStatus() {
  const { data } = await api.get('/status')
  return data as Status
}

export async function fetchReplay(callId: number) {
  const { data } = await api.post('/replay', { call_id: callId })
  return data
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
