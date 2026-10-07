<template>
  <form class="space-y-3 border-b border-gray-200 pb-4 dark:border-dark-600" @submit.prevent="submit">
    <h3 class="text-sm font-semibold">原 IP 自动重新授权</h3>
    <label class="block">
      <span class="input-label">账号邮箱</span>
      <input v-model="email" class="input w-full" type="email" autocomplete="off"
        maxlength="254" required :disabled="busy" />
    </label>
    <label class="block">
      <span class="input-label">账号密码</span>
      <input v-model="password" class="input w-full" type="password" autocomplete="new-password"
        maxlength="4096" required :disabled="busy" />
    </label>
    <label class="block">
      <span class="input-label">2FA 秘密（可选）</span>
      <input v-model="totpSecret" class="input w-full" type="password" autocomplete="new-password"
        maxlength="256" :disabled="busy" />
    </label>
    <p v-if="error" role="alert" class="text-sm text-red-600 break-words">{{ error }}</p>
    <p v-if="running" role="status" class="text-sm">正在等待授权浏览器，最长 5 分钟</p>
    <div class="flex flex-wrap gap-2">
      <button type="submit" class="btn btn-primary" :disabled="busy || !email.trim() || !password">
        自动重新授权并覆盖
      </button>
      <button v-if="running" type="button" class="btn btn-secondary" @click="$emit('cancel')">取消自动授权</button>
    </div>
  </form>
</template>

<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import type { AuthBrowserLogin } from '@/api/admin/accounts'
const props = defineProps<{ busy: boolean; running: boolean; error: string; generation: number }>()
const emit = defineEmits<{ submit: [login: AuthBrowserLogin]; cancel: [] }>()
const email = ref('')
const password = ref('')
const totpSecret = ref('')
const clear = () => { email.value = password.value = totpSecret.value = '' }
watch(() => props.generation, clear, { flush: 'sync' })
onBeforeUnmount(clear)
const submit = () => {
  if (props.busy) return
  const login = { email: email.value.trim(), password: password.value, totp_secret: totpSecret.value.trim() }
  clear()
  emit('submit', login)
}
</script>
