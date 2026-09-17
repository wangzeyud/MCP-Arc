<template>
  <el-container style="height: 100vh">
    <el-header style="display: flex; align-items: center; gap: 10px">
      <h2 style="margin: 0">MCP Arc Console</h2>
    </el-header>
    <el-container>
      <el-aside width="200px" style="border-right: 1px solid #eee">
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
  </el-container>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import Dashboard from './views/Dashboard.vue'
import Logs from './views/Logs.vue'
import Rules from './views/Rules.vue'

const active = ref('dashboard')
</script>
