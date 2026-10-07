<template>
  <ChannelStatusV1View v-if="isV1" />
  <ChannelStatusV2View v-else-if="isDetailed" />
  <ChannelStatusOverview v-else />
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { isChannelMonitorV1Mode } from '@/utils/featureFlags'
import ChannelStatusV1View from './ChannelStatusV1View.vue'
import ChannelStatusV2View from './ChannelStatusV2View.vue'
import ChannelStatusOverview from './ChannelStatusOverview.vue'

const route = useRoute()
// Existing analysis bookmarks keep their meaning; new visits open the cards.
const isDetailed = computed(() => {
  if (route.query.monitor_view === 'cards') return false
  return ['details', 'v2'].includes(String(route.query.monitor_view)) ||
    ['group_by', 'health_mode', 'trend_view', 'tab', 'platform', 'group', 'model'].some(key => route.query[key] != null)
})

const isV1 = computed(() => isChannelMonitorV1Mode())
</script>
