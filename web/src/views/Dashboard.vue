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

    <el-row :gutter="16">
      <el-col :span="8">
        <el-card><el-statistic title="Total Calls" :value="stats.total_calls" /></el-card>
      </el-col>
      <el-col :span="8">
        <el-card><el-statistic title="Errors" :value="stats.error_count" /></el-card>
      </el-col>
      <el-col :span="8">
        <el-card><el-statistic title="Avg Latency (ms)" :value="stats.avg_latency_ms" /></el-card>
      </el-col>
    </el-row>
    <el-card style="margin-top: 16px">
      <template #header>Calls per tool</template>
      <ul>
        <li v-for="(c, name) in stats.tool_counts" :key="name">{{ name }}: {{ c }}</li>
        <li v-if="!stats.tool_counts || !Object.keys(stats.tool_counts).length">No data yet</li>
      </ul>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { fetchStats, fetchStatus, type Stats, type Status } from '../api'

const stats = ref<Stats>({ total_calls: 0, error_count: 0, avg_latency_ms: 0, tool_counts: {} })
const status = ref<Status>({ client_transport: '', sse_url: '', console_url: '', admin_port: 0 })

function copy(text: string) {
  if (text) navigator.clipboard?.writeText(text)
}

onMounted(async () => {
  stats.value = await fetchStats()
  try {
    status.value = await fetchStatus()
  } catch {
    // Endpoint may be absent on older builds; the banner simply stays hidden.
  }
})
</script>
