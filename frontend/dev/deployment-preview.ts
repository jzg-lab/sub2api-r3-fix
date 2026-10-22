import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { i18n, loadLocaleMessages } from '@/i18n'
import Preview from './DeploymentPreview.vue'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'
import '@/style.css'

await loadLocaleMessages('zh')
i18n.global.locale.value = 'zh'
const pinia = createPinia()
// In-memory preview identities only; no login, credentials or persistent session.
useAuthStore(pinia).$patch({ user: { role: 'admin' } })
useAppStore(pinia).$patch({ currentVersion: '0.2.17+r17.1', latestVersion: '0.2.17+r17.2', hasUpdate: true, versionLoaded: true })
createApp(Preview).use(pinia).use(i18n).mount('#app')
