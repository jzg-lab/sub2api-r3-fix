<template>
  <div class="preview-label" role="status"><Icon name="beaker" size="sm" />本地演示环境 · 不连接生产 · 验证码 123456</div>
  <header class="preview-header">
    <div class="flex min-w-0 items-center gap-3"><img src="/logo.svg" alt="Sub2API" width="32" height="32" /><span class="text-lg font-semibold">Sub2API</span><span class="hidden text-sm text-gray-500 sm:inline">系统管理</span></div>
    <div class="flex min-w-0 items-center gap-2"><VersionBadge :key="generation" /><button type="button" class="btn btn-ghost h-9 w-9 p-2" :aria-label="dark ? '切换浅色' : '切换深色'" :title="dark ? '切换浅色' : '切换深色'" @click="toggleTheme"><Icon :name="dark ? 'sun' : 'moon'" size="sm" /></button></div>
  </header>
  <main class="preview-main">
    <div class="mb-6 flex flex-wrap items-center justify-between gap-4"><div><p class="mb-2 text-xs text-gray-500 dark:text-dark-400">系统 / 版本发布</p><h1 class="text-2xl font-semibold">部署验收</h1></div><button class="btn btn-primary" type="button" @click="show = true"><Icon name="download" size="sm" />部署更新</button></div>
    <dl class="preview-facts">
      <div><dt>运行环境</dt><dd>本机隔离预览</dd></div>
      <div><dt>演示页面版本</dt><dd class="font-mono">v0.2.17+r17.1</dd></div>
      <div><dt>部署方式</dt><dd>短暂维护，仅换应用</dd></div>
      <div><dt>真实执行器</dt><dd class="text-amber-700 dark:text-amber-300">未连接</dd></div>
    </dl>
    <div class="flex flex-wrap items-center gap-3 py-6">
      <label for="scenario" class="text-sm font-medium">测试场景</label>
      <Select id="scenario" v-model="scenario" :options="scenarios" :searchable="false" aria-label="测试场景" class="w-64 max-w-full" />
      <button class="btn btn-secondary h-10 w-10 p-2" type="button" title="重置场景" aria-label="重置场景" @click="reset"><Icon name="refresh" size="sm" /></button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <DeploymentDialog :key="generation" :show="show" @close="show = false" />
    <Toast />
  </main>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import DeploymentDialog from '@/components/common/DeploymentDialog.vue'
import VersionBadge from '@/components/common/VersionBadge.vue'
import Icon from '@/components/icons/Icon.vue'
import Toast from '@/components/common/Toast.vue'
import Select from '@/components/common/Select.vue'

const show = ref(true)
const scenario = ref('normal')
const scenarios = [
  { value: 'normal', label: '正常更新' },
  { value: 'previous-version', label: '上一版可回退' },
  { value: 'failed', label: '准备失败，旧版不动' },
  { value: 'expired', label: '备份过期' },
  { value: 'auto-rollback', label: '新版故障，恢复旧版' },
  { value: 'long-session', label: '请求未排空，取消更新' }
]
const generation = ref(0)
const error = ref('')
const dark = ref(false)
function toggleTheme() {
  dark.value = !dark.value
  document.documentElement.classList.toggle('dark', dark.value)
}
async function reset() {
  try {
    const response = await fetch('/__preview/reset', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ scenario: scenario.value }) })
    if (!response.ok) throw new Error('preview reset failed')
    error.value = ''
    generation.value++
    show.value = true
  } catch { error.value = '本地预览连接失败' }
}
</script>

<style>
.preview-label { @apply pointer-events-none fixed inset-x-0 top-0 flex min-h-10 items-center justify-center gap-2 border-b border-amber-200 bg-amber-50 px-3 py-2 text-center text-xs text-amber-800 dark:border-amber-900 dark:bg-dark-900 dark:text-amber-300; z-index: 80; }
.preview-header { @apply mt-10 flex min-h-[72px] flex-wrap items-center justify-between gap-3 border-b border-gray-200 bg-white px-4 py-4 dark:border-dark-700 dark:bg-dark-900 sm:px-6; }
.preview-main { @apply mx-auto mt-7 max-w-5xl px-4 sm:mt-10 sm:px-6; }
.preview-facts { @apply grid grid-cols-2 gap-5 border-y border-gray-200 py-6 dark:border-dark-700 sm:grid-cols-4 sm:gap-6; }
.preview-facts dt { @apply mb-2 text-xs text-gray-500 dark:text-dark-400; }
.preview-facts dd { @apply break-words text-sm; }
body .modal-overlay { padding: 64px 12px 16px; }
body .modal-content { max-height: calc(100dvh - 80px); }
</style>
