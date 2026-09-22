<template>
  <el-container style="height: 100vh">
    <el-header
      style="
        display: flex;
        align-items: center;
        gap: 10px;
        border-bottom: 1px solid #eee;
        background: #fff;
      "
    >
      <h2 style="margin: 0; font-size: 18px">MCP Arc Console</h2>
      <el-tag size="small" type="info">{{ active }}</el-tag>
      <el-tag v-if="status.admin_port" size="small" effect="plain">
        :{{ status.admin_port }}
      </el-tag>
      <el-tag
        v-if="status.pid"
        size="small"
        effect="plain"
        title="Process serving this console. Two instances may run side by side on different ports."
      >
        pid {{ status.pid }}
      </el-tag>
      <el-tag
        v-if="status.audit_driver"
        size="small"
        effect="plain"
        :type="status.audit_driver === 'memory' ? 'warning' : 'success'"
        :title="
          status.audit_driver === 'memory'
            ? '内存审计：进程退出即清空（这也是重启后控制台看起来空着的原因）。把 audit.driver 改成 sqlite 即可持久化。'
            : '审计记录已持久化到数据库。'
        "
      >
        audit: {{ status.audit_driver }}{{ status.audit_driver === 'memory' ? ' (非持久)' : '' }}
      </el-tag>
      <span v-if="uptime" style="font-size: 12px; color: #999">up {{ uptime }}</span>
      <div style="flex: 1" />
      <el-button size="small" @click="openSettings">Token</el-button>
    </el-header>
    <el-container>
      <el-aside width="200px" style="border-right: 1px solid #eee; background: #fff">
        <el-menu :default-active="active" @select="(k: string) => (active = k)">
          <el-menu-item index="dashboard">Dashboard</el-menu-item>
          <el-menu-item index="logs">Call Logs</el-menu-item>
          <el-menu-item index="rules">Rules</el-menu-item>
        </el-menu>
      </el-aside>
      <el-main>
        <Dashboard v-if="active === 'dashboard'" />
        <Logs v-else-if="active === 'logs'" />
        <Rules v-else-if="active === 'rules'" />
      </el-main>
    </el-container>

    <el-dialog v-model="settingsVisible" title="Console access token" width="420px">
      <p style="color: #888; font-size: 12px; margin-top: 0">
        Must match the <code>admin.token</code> in your mcp-arc config. Changes take effect after a reload.
      </p>
      <el-input v-model="tokenInput" placeholder="e.g. change-me" @keyup.enter="saveToken" />
      <template #footer>
        <el-button @click="settingsVisible = false">Cancel</el-button>
        <el-button type="primary" @click="saveToken">Save &amp; reload</el-button>
      </template>
    </el-dialog>
  </el-container>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import Dashboard from './views/Dashboard.vue'
import Logs from './views/Logs.vue'
import Rules from './views/Rules.vue'
import { fetchStatus, type Status } from './api'

const active = ref('dashboard')
const settingsVisible = ref(false)
const tokenInput = ref(localStorage.getItem('mcp_arc_token') || 'change-me')

// Identity of the process behind this page: PID, admin port and audit backend.
// Several mcp-arc instances can be live at once, so without this a stale tab is
// indistinguishable from a quiet one.
const status = ref<Partial<Status>>({})
const uptime = computed(() => {
  if (!status.value.started_at) return ''
  const secs = Math.max(0, Math.round((Date.now() - new Date(status.value.started_at).getTime()) / 1000))
  if (secs < 60) return `${secs}s`
  if (secs < 3600) return `${Math.floor(secs / 60)}m`
  return `${Math.floor(secs / 3600)}h${Math.floor((secs % 3600) / 60)}m`
})

onMounted(async () => {
  try {
    status.value = await fetchStatus()
  } catch {
    // Older builds may not expose /api/status; the chips simply stay hidden.
  }
})

function openSettings() {
  tokenInput.value = localStorage.getItem('mcp_arc_token') || 'change-me'
  settingsVisible.value = true
}

function saveToken() {
  const t = tokenInput.value.trim()
  if (!t) {
    ElMessage.warning('Token cannot be empty')
    return
  }
  localStorage.setItem('mcp_arc_token', t)
  settingsVisible.value = false
  // The axios client reads the token once at module load, so reload to apply.
  location.reload()
}
</script>
