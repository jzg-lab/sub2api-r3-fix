<template>
  <header class="plaza-nav">
    <RouterLink :to="backTarget" class="plaza-brand">
      <img :src="siteLogo || '/logo.svg'" alt="" />
      <strong>{{ siteName }}</strong>
    </RouterLink>
    <span class="plaza-nav-divider" aria-hidden="true"></span>
    <h1>{{ t('modelPlaza.title') }}</h1>
    <RouterLink class="plaza-back" :to="isAuthenticated ? backTarget : { path: '/login', query: { redirect: '/model-plaza' } }">
      <Icon name="chevronLeft" size="sm" />
      {{ t(isAuthenticated ? 'modelPlaza.nav.backToDashboard' : 'modelPlaza.nav.login') }}
    </RouterLink>
  </header>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { sanitizeUrl } from '@/utils/url'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
const { t } = useI18n()
const appStore = useAppStore()
const authStore = useAuthStore()
const siteName = computed(() => appStore.cachedPublicSettings?.site_name || 'Sub2API')
const siteLogo = computed(() => sanitizeUrl(appStore.cachedPublicSettings?.site_logo || '', { allowRelative: true, allowDataUrl: true }))
const isAuthenticated = computed(() => authStore.isAuthenticated)
const backTarget = computed(() => authStore.isAdmin ? '/admin/dashboard' : '/dashboard')
</script>
