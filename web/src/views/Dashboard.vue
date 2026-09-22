<template>
  <div>
    <el-alert
      v-if="status.client_transport === 'sse'"
      type="success"
      :closable="false"
      style="margin-bottom: 16px"
    >
      <template #title>
        <span>Add as an MCP server (SSE): </span>
        <code style="background: #f5f5f5; padding: 2px 6px; border-radius: 4px">{{ status.sse_url }}</code>
        <el-button size="small" style="margin-left: 8px" @click="copy(status.sse_url)">Copy</el-button>
      </template>
    </el-alert>

    <el-alert
      v-if="error"
      type="error"
      :closable="false"
      :title="error"
      style="margin-bottom: 16px"
    />

    <div style="margin-bottom: 12px; display: flex; align-items: center; gap: 10px; flex-wrap: wrap">
      <el-button size="small" :loading="loading" @click="load">Refresh</el-button>
      <el-switch
        v-model="autoRefresh"
        inline-prompt
        active-text="On"
        inactive-text="Off"
        @change="toggleAuto"
      />
      <span style="font-size: 12px; color: #909399">
        <template v-if="lastUpdated">updated {{ lastUpdated }} · auto every 5s while On</template>
        <template v-else>this page does not poll on its own — press Refresh to pick up new calls</template>
      </span>
    </div>

    <el-row :gutter="16">
      <el-col :span="6">
        <el-card><el-statistic title="Total Calls" :value="stats.total_calls" /></el-card>
      </el-col>
      <el-col :span="6">
        <el-card>
          <el-statistic title="Errors" :value="stats.error_count" />
          <div style="margin-top: 4px; font-size: 12px; color: #c0392b" v-if="stats.error_count">
            {{ errorRate }}% of calls failed
          </div>
        </el-card>
      </el-col>
      <el-col :span="6">
        <el-card><el-statistic title="Error Rate" :value="errorRate" suffix="%" /></el-card>
      </el-col>
      <el-col :span="6">
        <el-card>
          <el-statistic title="Avg Latency (ms)" :value="round1(stats.avg_latency_ms)" />
          <div v-if="subMillisecond" style="margin-top: 4px; font-size: 12px; color: #909399">
            sub-millisecond calls record as 0
          </div>
        </el-card>
      </el-col>
    </el-row>

    <el-row :gutter="16" style="margin-top: 16px">
      <el-col :span="8">
        <el-card><el-statistic title="Distinct Tools" :value="distinctTools" /></el-card>
      </el-col>
      <el-col :span="16">
        <el-card style="height: 100%">
          <template #header>Calls per tool</template>
          <div v-if="!toolRows.length" style="color: #909399; font-size: 13px">No data yet</div>
          <div v-for="t in toolRows" :key="t.name" style="margin-bottom: 8px">
            <div style="display: flex; justify-content: space-between; font-size: 13px">
              <span>{{ t.name }}</span>
              <span style="color: #606266">{{ t.count }}</span>
            </div>
            <el-progress :percentage="t.pct" :show-text="false" :stroke-width="8" />
          </div>
        </el-card>
      </el-col>
    </el-row>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { errorText, fetchStats, fetchStatus, type Stats, type Status } from '../api'

const stats = ref<Stats>({ total_calls: 0, error_count: 0, avg_latency_ms: 0, tool_counts: {} })
const status = ref<Partial<Status>>({})
const loading = ref(false)
const error = ref('')
const lastUpdated = ref('')
const autoRefresh = ref(false)
let timer: number | null = null

const errorRate = computed(() => {
  if (!stats.value.total_calls) return 0
  return Math.round((stats.value.error_count / stats.value.total_calls) * 1000) / 10
})
const distinctTools = computed(() => Object.keys(stats.value.tool_counts || {}).length)
// A fast local tool answers in well under a millisecond, so latency_ms is 0 and
// an unqualified "0 ms" reads like a broken metric. Flag it instead.
const subMillisecond = computed(() => stats.value.total_calls > 0 && stats.value.avg_latency_ms === 0)

const toolRows = computed(() => {
  const entries = Object.entries(stats.value.tool_counts || {})
  if (!entries.length) return []
  const max = Math.max(...entries.map(([, c]) => c))
  return entries
    .sort((a, b) => b[1] - a[1])
    .map(([name, count]) => ({ name, count, pct: max ? Math.round((count / max) * 100) : 0 }))
})

const round1 = (n: number) => Math.round((n || 0) * 10) / 10

function copy(text: string) {
  if (text) navigator.clipboard?.writeText(text)
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    stats.value = await fetchStats()
    lastUpdated.value = new Date().toLocaleTimeString()
  } catch (e: any) {
    // Never leave the cards silently at 0: that is indistinguishable from having
    // no traffic at all, which is exactly the confusion this page used to cause.
    error.value = errorText(e) || 'Failed to load metrics'
    lastUpdated.value = ''
  } finally {
    loading.value = false
  }
}

async function loadStatus() {
  try {
    status.value = await fetchStatus()
  } catch {
    // Endpoint may be absent on older builds; the banner simply stays hidden.
  }
}

function toggleAuto() {
  if (autoRefresh.value) {
    timer = window.setInterval(load, 5000)
  } else if (timer) {
    clearInterval(timer)
    timer = null
  }
}

onMounted(() => {
  load()
  loadStatus()
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>
