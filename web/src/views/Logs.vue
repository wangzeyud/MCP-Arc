<template>
  <div>
    <div style="margin-bottom: 12px; display: flex; gap: 8px; align-items: center; flex-wrap: wrap">
      <el-button size="small" :loading="exporting" @click="doExport('json')">
        Export JSON
      </el-button>
      <el-button size="small" :loading="exporting" @click="doExport('csv')">
        Export CSV
      </el-button>
      <el-checkbox v-model="includeRaw" label="include raw (unmasked)" />
      <el-divider direction="vertical" />
      <el-checkbox v-model="withDiff" label="diff vs recorded" />
      <el-button
        size="small"
        type="primary"
        :disabled="selected.length === 0"
        :loading="batchBusy"
        @click="replaySelected"
      >
        Replay selected ({{ selected.length }})
      </el-button>
      <el-button size="small" :loading="loading" @click="reload">Refresh</el-button>
      <el-switch
        v-model="autoRefresh"
        inline-prompt
        active-text="On"
        inactive-text="Off"
        @change="toggleAuto"
        style="margin-left: 4px"
      />
    </div>

    <el-table
      :data="rows"
      height="600"
      style="width: 100%"
      row-key="id"
      :row-class-name="rowClass"
      @selection-change="(rows: CallRecord[]) => (selected = rows)"
    >
      <el-table-column type="selection" width="48" />
      <el-table-column prop="id" label="ID" width="80" />
      <el-table-column label="Type" width="96">
        <template #default="{ row }">
          <el-tag v-if="row.replay_of" size="small" type="info">↺ replay</el-tag>
          <span v-else>—</span>
        </template>
      </el-table-column>
      <el-table-column prop="client_id" label="Client" width="120" />
      <el-table-column prop="tool_name" label="Tool" width="160" />
      <el-table-column prop="timestamp" label="Time" width="220" />
      <el-table-column label="Latency" width="100">
        <template #default="{ row }">
          {{ row.latency_ms > 0 ? row.latency_ms : '<1' }}
        </template>
      </el-table-column>
      <el-table-column label="Error" width="160">
        <template #default="{ row }">
          <span v-if="row.error_msg" style="color: #c0392b; font-size: 12px">
            {{ preview(row.error_msg, 40) }}
          </span>
          <span v-else style="color: #67c23a">ok</span>
        </template>
      </el-table-column>
      <el-table-column label="Params / Result">
        <template #default="{ row }">
          <el-popover trigger="hover" width="480" placement="left">
            <pre style="white-space: pre-wrap; max-height: 340px; overflow: auto">{{ row.params }}

--- RESULT ---
{{ row.result }}

--- ERROR ---
{{ row.error_msg || '(none)' }}</pre>
            <template #reference><span>{{ preview(row.params) }}</span></template>
          </el-popover>
        </template>
      </el-table-column>
      <el-table-column label="Replay" width="100">
        <template #default="{ row }">
          <el-button size="small" :loading="busy === row.id" @click="replay(row)">Replay</el-button>
        </template>
      </el-table-column>
      <template #empty>
        <el-empty description="No calls recorded yet — trigger a tool from your MCP client." />
      </template>
    </el-table>

    <el-dialog v-model="dialogVisible" title="Replay response" width="70%">
      <div v-if="replayData">
        <div style="margin-bottom: 8px; display: flex; align-items: center; gap: 8px">
          <strong>{{ replayData.tool }}</strong>
          <el-button size="small" @click="copyText(pretty(replayedPayload))">Copy</el-button>
        </div>

        <el-alert
          v-if="replayData.diff"
          :type="replayData.diff.match ? 'success' : 'warning'"
          :closable="false"
          style="margin-bottom: 12px"
        >
          <template #title>
            {{
              replayData.diff.match
                ? 'Match — replayed response equals the recorded result'
                : 'Changed — replayed response differs from the recorded result'
            }}
          </template>
        </el-alert>

        <el-row v-if="replayData.diff && !replayData.diff.match" :gutter="12">
          <el-col :span="12">
            <div class="panel-title">Recorded result</div>
            <pre class="code">{{ pretty(recordedPayload) }}</pre>
          </el-col>
          <el-col :span="12">
            <div class="panel-title">Replayed result</div>
            <pre class="code">{{ pretty(replayedPayload) }}</pre>
          </el-col>
        </el-row>
        <pre v-else class="code">{{ pretty(replayedPayload) }}</pre>
      </div>
    </el-dialog>

    <el-dialog v-model="batchVisible" title="Batch replay results" width="75%">
      <el-table :data="batchResults" height="440" style="width: 100%">
        <el-table-column prop="call_id" label="ID" width="80" />
        <el-table-column prop="tool" label="Tool" width="160" />
        <el-table-column prop="status" label="Status" width="120" />
        <el-table-column label="Diff" width="120">
          <template #default="{ row }">
            <el-tag v-if="row.diff" :type="row.diff.match ? 'success' : 'danger'" size="small">
              {{ row.diff.match ? 'match' : 'changed' }}
            </el-tag>
            <span v-else>—</span>
          </template>
        </el-table-column>
        <el-table-column label="Response / Error">
          <template #default="{ row }">
            <span style="color: #c0392b" v-if="row.error">{{ row.error }}</span>
            <pre
              v-else
              style="white-space: pre-wrap; max-height: 220px; overflow: auto; margin: 0"
            >{{ pretty(extractPayload(row.response)) }}</pre>
          </template>
        </el-table-column>
      </el-table>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import {
  downloadBlob,
  errorText,
  exportCalls,
  extractPayload,
  fetchLogs,
  fetchReplay,
  fetchReplayBatch,
  type CallRecord,
  type ReplayItem,
  type ReplayResult,
} from '../api'

const rows = ref<CallRecord[]>([])
const loading = ref(false)
const dialogVisible = ref(false)
const replayData = ref<ReplayResult | null>(null)
const busy = ref<number | null>(null)
const exporting = ref(false)
const includeRaw = ref(false)
const withDiff = ref(false)
const selected = ref<CallRecord[]>([])
const batchBusy = ref(false)
const batchVisible = ref(false)
const batchResults = ref<ReplayItem[]>([])
const autoRefresh = ref(false)
const autoTimer = ref<number | null>(null)

const EXPORT_LIMIT = 1000

const preview = (s: string, n = 48) => (s && s.length > n ? s.slice(0, n) + '…' : s || '')
const pretty = (v: any) => (v == null ? '' : typeof v === 'string' ? v : JSON.stringify(v, null, 2))
const replayedPayload = computed(() =>
  replayData.value ? extractPayload(replayData.value.response) : null,
)
const recordedPayload = computed(() =>
  replayData.value?.recorded != null ? extractPayload(replayData.value.recorded) : null,
)

async function reload() {
  loading.value = true
  try {
    rows.value = await fetchLogs({ limit: 200 })
  } finally {
    loading.value = false
  }
}

async function doExport(format: 'json' | 'csv') {
  exporting.value = true
  try {
    const params: Record<string, any> = { limit: EXPORT_LIMIT }
    if (includeRaw.value) params.raw = 1
    const blob = await exportCalls(format, params)
    const stamp = new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')
    downloadBlob(blob, `mcp-arc-calls-${stamp}.${format}`)
  } catch (e: any) {
    const msg = errorText(e)
    if (msg) ElMessage.error(msg)
  } finally {
    exporting.value = false
  }
}

async function replay(row: CallRecord) {
  busy.value = row.id
  try {
    replayData.value = await fetchReplay(row.id, withDiff.value)
    dialogVisible.value = true
  } catch (e: any) {
    const msg = errorText(e)
    if (msg) ElMessage.error(msg)
  } finally {
    busy.value = null
  }
}

async function replaySelected() {
  if (selected.value.length === 0) return
  batchBusy.value = true
  try {
    const data = await fetchReplayBatch({
      call_ids: selected.value.map((r) => r.id),
      diff: withDiff.value,
    })
    batchResults.value = data.results
    batchVisible.value = true
  } catch (e: any) {
    const msg = errorText(e)
    if (msg) ElMessage.error(msg)
  } finally {
    batchBusy.value = false
  }
}

function rowClass({ row }: { row: CallRecord }) {
  return row.error_msg ? 'replay-error-row' : ''
}

function copyText(text: string) {
  navigator.clipboard
    ?.writeText(text)
    .then(() => ElMessage.success('Copied'), () => ElMessage.error('Copy failed'))
}

function toggleAuto() {
  if (autoRefresh.value) {
    autoTimer.value = window.setInterval(reload, 5000)
  } else if (autoTimer.value) {
    clearInterval(autoTimer.value)
    autoTimer.value = null
  }
}

onMounted(reload)
onUnmounted(() => {
  if (autoTimer.value) clearInterval(autoTimer.value)
})
</script>

<style scoped>
.code {
  white-space: pre-wrap;
  max-height: 420px;
  overflow: auto;
  background: #fafafa;
  border: 1px solid #eee;
  border-radius: 6px;
  padding: 10px;
  margin: 0;
  font-size: 12px;
}
.panel-title {
  font-size: 12px;
  color: #888;
  margin-bottom: 4px;
}
:deep(tr.replay-error-row td) {
  background: #fef0f0 !important;
}
</style>
