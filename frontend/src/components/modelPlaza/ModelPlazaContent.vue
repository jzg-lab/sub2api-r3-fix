<template>
  <div v-if="loading && !response" class="plaza-empty" role="status">{{ t('modelPlaza.loading') }}</div>
  <div v-else-if="!response" class="plaza-empty" role="alert">
    <p>{{ t(error ? 'modelPlaza.loadFailed' : 'modelPlaza.empty') }}</p>
    <button class="plaza-button" @click="$emit('retry')">{{ t('modelPlaza.catalog.retry') }}</button>
  </div>
  <div v-else class="plaza-layout">
    <aside class="plaza-filters" :class="{ open: filtersOpen }" :aria-label="t('modelPlaza.catalog.filters')">
      <div class="plaza-filter-heading">
        <h2>{{ t('modelPlaza.catalog.filters') }}</h2>
        <button class="plaza-text-button" @click="reset">{{ t('modelPlaza.catalog.reset') }}</button>
        <button class="plaza-icon-button plaza-mobile" :aria-label="t('modelPlaza.catalog.closeFilters')" @click="filtersOpen = false"><Icon name="x" size="sm" /></button>
      </div>
      <p class="plaza-filter-caption">{{ t('modelPlaza.catalog.filterHint') }}</p>
      <template v-for="section in filterSections" :key="section.key">
        <details open class="plaza-filter-section">
          <summary>{{ section.label }}</summary>
          <div class="plaza-chips" :class="{ 'plaza-provider-chips': section.key === 'platform' }">
            <button class="plaza-chip" :aria-pressed="filters[section.key] === ''" @click="filters[section.key] = ''">
              {{ t('modelPlaza.filters.all') }}<small v-if="section.key === 'platform'">{{ allEntries.length }}</small>
            </button>
            <button v-for="option in section.options" :key="option.value" class="plaza-chip" :aria-pressed="filters[section.key] === option.value" @click="filters[section.key] = option.value">
              <ProviderIcon v-if="section.key === 'platform'" class="plaza-provider-icon" :data-provider="option.value" :provider="option.value" :size="16" aria-hidden="true" />
              <span>{{ option.label }}</span><small>{{ option.count }}</small>
            </button>
          </div>
        </details>
        <details v-if="section.key === 'platform'" open class="plaza-filter-section">
          <summary>{{ t('modelPlaza.filters.groupLabel') }}</summary>
          <div class="plaza-chips">
            <button class="plaza-chip" :aria-pressed="selectedGroup === 'all'" @click="selectedGroup = 'all'">{{ t('modelPlaza.filters.all') }}</button>
            <button v-for="g in visibleGroups" :key="g.id" class="plaza-chip" :aria-pressed="selectedGroup === g.id" @click="selectedGroup = g.id">{{ g.name }}<small>×{{ g.user_rate_multiplier ?? g.rate_multiplier }}</small></button>
          </div>
        </details>
      </template>
      <p class="plaza-filter-note">{{ t('modelPlaza.catalog.groupHint') }}</p>
    </aside>

    <section class="plaza-catalog" :aria-label="t('modelPlaza.title')">
      <div class="plaza-toolbar">
        <div class="plaza-result-count"><strong>{{ entries.length }}</strong><span>{{ t('modelPlaza.catalog.models') }}</span></div>
        <label class="plaza-search"><Icon name="search" size="sm" /><input v-model="search" :aria-label="t('modelPlaza.filters.searchPlaceholder')" :placeholder="t('modelPlaza.filters.searchPlaceholder')" /></label>
        <div class="plaza-toolbar-actions">
          <div class="plaza-segmented" :aria-label="t('modelPlaza.catalog.currency')">
            <button :aria-pressed="currency === 'CNY'" @click="currency = 'CNY'">CNY</button>
            <button :aria-pressed="currency === 'USD'" @click="currency = 'USD'">USD</button>
          </div>
          <button class="plaza-original-toggle" :aria-pressed="showOriginal" @click="showOriginal = !showOriginal">{{ t(showOriginal ? 'modelPlaza.catalog.showCurrent' : 'modelPlaza.catalog.showOriginal') }}</button>
          <select v-model="sort" :aria-label="t('modelPlaza.catalog.sort')"><option value="name">{{ t('modelPlaza.catalog.sortName') }}</option><option value="price">{{ t('modelPlaza.catalog.sortPrice') }}</option><option value="health">{{ t('modelPlaza.catalog.sortHealth') }}</option></select>
          <div class="plaza-segmented plaza-view-switch"><button :aria-pressed="view === 'grid'" :aria-label="t('modelPlaza.catalog.grid')" @click="view = 'grid'"><Icon name="grid" size="sm" /></button><button :aria-pressed="view === 'list'" :aria-label="t('modelPlaza.catalog.list')" @click="view = 'list'"><Icon name="menu" size="sm" /></button></div>
          <button class="plaza-icon-button plaza-mobile" :aria-expanded="filtersOpen" :aria-label="t('modelPlaza.catalog.filters')" @click="filtersOpen = !filtersOpen"><Icon name="filter" size="sm" /></button>
        </div>
      </div>
      <div class="plaza-catalog-meta">
        <span>{{ t(currency === 'CNY' ? 'modelPlaza.catalog.cny' : 'modelPlaza.catalog.usd') }} · {{ unitLabel }} · {{ t(showOriginal ? 'modelPlaza.catalog.referenceTitle' : 'modelPlaza.catalog.regular') }}</span>
        <span v-if="error" class="plaza-stale" role="status">{{ t('modelPlaza.catalog.stale') }} <button @click="$emit('retry')">{{ t('modelPlaza.catalog.retry') }}</button></span>
        <span v-else :title="updatedAt?.toLocaleTimeString()" class="plaza-live"><i></i>{{ t('modelPlaza.catalog.live') }}</span>
      </div>
      <div v-if="showOriginal" class="plaza-notice">{{ referenceHint }}</div>
      <details v-if="descriptionHtml" class="plaza-description"><summary>{{ t('modelPlaza.catalog.priceNotes') }}</summary><div v-html="descriptionHtml"></div></details>

      <div v-if="entries.length" class="plaza-model-grid" :class="{ 'plaza-list': view === 'list' }">
        <article v-for="entry in entries" :key="entry.key" class="plaza-model-card">
          <div class="plaza-model-top">
            <span class="plaza-model-logo"><ModelIcon :model="offer(entry).model.name" size="29px" /></span>
            <div class="plaza-model-heading"><h2>{{ offer(entry).model.name }}</h2><span>{{ platformDisplayName(offer(entry).model.platform) }}</span></div>
            <button class="plaza-icon-button" :aria-label="t('modelPlaza.catalog.copyModel', { name: offer(entry).model.name })" @click="copy(offer(entry).model.name)"><Icon name="copy" size="sm" /></button>
          </div>
          <p class="plaza-model-description">{{ offer(entry).group.description || t('modelPlaza.catalog.configuredPrice') }}</p>
          <div class="plaza-model-tags"><span v-if="offer(entry).group.is_exclusive">{{ t('modelPlaza.badges.exclusive') }}</span><span v-if="offer(entry).group.subscription_type === 'subscription'">{{ t('modelPlaza.badges.subscription') }}</span><span>{{ modeLabel(offer(entry).model) }}</span><span v-if="pricingRows(offer(entry).model, showOriginal).length > 1">{{ t('modelPlaza.catalog.tiered') }}</span><span v-if="!showOriginal && (offer(entry).model.time_pricing || offer(entry).group.peak_rate_enabled)">{{ t('modelPlaza.catalog.timed') }}</span></div>
          <div class="plaza-pricing-type">{{ priceTitle }}<small>{{ priceUnit(offer(entry).model) }}<template v-if="firstTierLabel(offer(entry).model)"> · {{ firstTierLabel(offer(entry).model) }}</template></small></div>
          <PlazaPriceGrid :offer="offer(entry)" :currency="currency" :conversion="response.balance_recharge_multiplier" :show-original="showOriginal" />
          <div class="plaza-model-meta"><label>{{ t('modelPlaza.filters.groupLabel') }}<select :value="offer(entry).group.id" :aria-label="t('modelPlaza.filters.groupLabel') + ' ' + offer(entry).model.name" @change="chosenGroups[entry.key] = Number(($event.target as HTMLSelectElement).value)"><option v-for="item in entry.offers" :key="item.group.id" :value="item.group.id">{{ item.group.name }}</option></select></label><span>{{ protocol(offer(entry).model) }}</span></div>
          <PlazaStatus :row="status(offer(entry))" :coverage="monitor?.coverage" @details="openDetail(entry.key, 'status')" />
          <button class="plaza-card-price-detail" @click="openDetail(entry.key, 'pricing')">{{ t('modelPlaza.catalog.priceDetails') }} <Icon name="chevronRight" size="xs" /></button>
        </article>
      </div>
      <div v-else class="plaza-empty"><p>{{ t('modelPlaza.noSearchResult') }}</p><button class="plaza-button" @click="reset">{{ t('modelPlaza.catalog.reset') }}</button></div>
      <footer class="plaza-footer">
        <div class="plaza-legend"><span v-for="state in ['healthy', 'warning', 'critical', 'unknown']" :key="state"><i :class="state"></i>{{ t(`modelPlaza.catalog.${state}`) }}</span></div>
        <span>{{ t(monitorUnavailable ? 'modelPlaza.catalog.monitorUnavailable' : 'modelPlaza.catalog.monitorWindow') }}</span>
      </footer>
      <p class="plaza-conversion-note">{{ conversionNote }}</p>
    </section>
  </div>

  <BaseDialog :show="!!detailEntry" :title="detailEntry ? offer(detailEntry).model.name : ''" width="wide" @close="detailKey = ''">
    <div v-if="detailEntry && response" class="plaza-detail">
      <div class="plaza-detail-tabs"><button :aria-pressed="detailTab === 'pricing'" @click="detailTab = 'pricing'">{{ t('modelPlaza.catalog.priceDetails') }}</button><button :aria-pressed="detailTab === 'status'" @click="detailTab = 'status'">{{ t('modelPlaza.catalog.statusDetails') }}</button></div>
      <template v-if="detailTab === 'pricing'">
        <p class="plaza-detail-note">{{ priceTitle }} · {{ offer(detailEntry).group.name }} · {{ priceUnit(offer(detailEntry).model) }} · {{ t(currency === 'CNY' ? 'modelPlaza.catalog.cny' : 'modelPlaza.catalog.usd') }}</p>
        <div class="plaza-detail-prices"><PlazaPriceGrid :offer="offer(detailEntry)" :currency="currency" :conversion="response.balance_recharge_multiplier" :show-original="showOriginal" /></div>
        <p v-if="error" class="plaza-stale">{{ t('modelPlaza.catalog.stale') }}</p>
        <h3>{{ t('modelPlaza.catalog.tiers') }}</h3>
        <p v-if="!pricingRows(offer(detailEntry).model, showOriginal).length" class="plaza-detail-note">{{ t('modelPlaza.catalog.priceUnavailable') }}</p>
        <div v-if="pricingRows(offer(detailEntry).model, showOriginal).length" class="plaza-detail-table-wrap"><table class="plaza-detail-table"><thead><tr><th>{{ t('modelPlaza.catalog.tier') }}</th><template v-if="billingMode(offer(detailEntry).model) === 'token'"><th v-for="field in priceFields" :key="field">{{ t(`modelPlaza.catalog.${field}`) }}</th><th>{{ t('modelPlaza.catalog.cache1h') }}</th></template><th v-else>{{ t('modelPlaza.catalog.per_request_price') }}</th></tr></thead><tbody>
          <tr v-for="(tier, i) in pricingRows(offer(detailEntry).model, showOriginal)" :key="i"><td>{{ 'tier_label' in tier && tier.tier_label || t('modelPlaza.catalog.baseTier') }}</td><template v-if="billingMode(offer(detailEntry).model) === 'token'"><td v-for="field in priceFields" :key="field">{{ formattedPrice(tier[field], offer(detailEntry), unit) }}</td><td>{{ formattedPrice(tier.cache_write_1h_price, offer(detailEntry), unit) }}</td></template><td v-else>{{ formattedPrice(tier.per_request_price, offer(detailEntry), 1) }}</td></tr>
        </tbody></table></div>
        <template v-if="!showOriginal">
          <p v-if="offer(detailEntry).model.pricing?.image_input_price != null" class="plaza-detail-note">{{ t('modelPlaza.catalog.imageInput') }}: {{ formattedPrice(offer(detailEntry).model.pricing?.image_input_price, offer(detailEntry), unit) }} / {{ unitLabel }}</p>
          <p v-if="offer(detailEntry).model.pricing?.image_output_price != null" class="plaza-detail-note">{{ t('modelPlaza.catalog.imageOutput') }}: {{ formattedPrice(offer(detailEntry).model.pricing?.image_output_price, offer(detailEntry), unit) }} / {{ unitLabel }}</p>
          <p v-if="offer(detailEntry).model.long_context_basis" class="plaza-detail-note">{{ t('modelPlaza.table.tierHint') }}</p>
          <p v-if="!offer(detailEntry).group.long_context_pricing_enabled && (offer(detailEntry).model.official_pricing?.intervals?.length ?? 0) > 1" class="plaza-detail-note">{{ t('modelPlaza.detail.longContextDisabledNote') }}</p>
          <p v-for="(multiplier, effort) in offer(detailEntry).model.pricing?.reasoning_effort_multipliers" :key="effort" class="plaza-detail-note">{{ t('modelPlaza.table.reasoningMultiplierHint', { effort, multiplier }) }}</p>
          <p v-if="offer(detailEntry).model.time_pricing" class="plaza-detail-note">{{ t('modelPlaza.catalog.timeSchedule') }} ({{ offer(detailEntry).model.time_pricing?.timezone }})<span v-for="period in offer(detailEntry).model.time_pricing?.periods" :key="period.start_time"> · {{ period.start_time }}–{{ period.end_time }} ×{{ period.multiplier }}</span><span v-if="offer(detailEntry).model.time_pricing?.weekdays_only"> · {{ t('modelPlaza.table.timePricingWeekdays') }}</span></p>
          <h3>{{ t('modelPlaza.catalog.groupComparison') }}</h3>
          <div class="plaza-detail-table-wrap"><table class="plaza-detail-table"><thead><tr><th>{{ t('modelPlaza.filters.groupLabel') }}</th><th>{{ t('modelPlaza.catalog.currentRate') }}</th><th>{{ t(billingMode(offer(detailEntry).model) === 'token' ? 'modelPlaza.catalog.input_price' : 'modelPlaza.catalog.per_request_price') }}</th><th>{{ t('modelPlaza.catalog.output_price') }}</th></tr></thead><tbody><tr v-for="item in allDetailOffers" :key="item.group.id"><td><button @click="chosenGroups[detailEntry.key] = item.group.id">{{ item.group.name }}</button></td><td>×{{ item.model.current_rate_multiplier ?? '—' }}</td><td>{{ formattedPrice(primaryPrice(item), item, billingMode(item.model) === 'token' ? unit : 1) }}</td><td>{{ billingMode(item.model) === 'token' ? formattedPrice(pricingRows(item.model)[0]?.output_price, item, unit) : '—' }}</td></tr></tbody></table></div>
          <p class="plaza-detail-note">{{ t('modelPlaza.catalog.requestOptionsHint') }}</p>
        </template>
        <p class="plaza-detail-note">{{ conversionNote }}</p>
      </template>
      <template v-else>
        <p class="plaza-detail-note">{{ offer(detailEntry).group.name }} · {{ platformDisplayName(offer(detailEntry).model.platform) }} · {{ t('modelPlaza.catalog.monitorWindow') }}</p>
        <PlazaStatus :row="status(offer(detailEntry))" :coverage="monitor?.coverage" expanded />
        <p class="plaza-detail-note">{{ t('modelPlaza.catalog.monitorHint') }}</p>
        <p v-if="monitor?.coverage.data_through" class="plaza-detail-note">{{ t('modelPlaza.catalog.dataThrough') }} {{ new Date(monitor.coverage.data_through).toLocaleString() }}</p>
        <p v-if="!status(offer(detailEntry))" class="plaza-detail-note">{{ t('modelPlaza.catalog.monitorUnavailable') }}</p>
      </template>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import Icon from '@/components/icons/Icon.vue'
import ModelIcon from '@/components/common/ModelIcon.vue'
import ProviderIcon from '@/components/user/monitor/ProviderIcon.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import PlazaPriceGrid from './PlazaPriceGrid.vue'
import PlazaStatus from './PlazaStatus.vue'
import type { ModelPlazaResponse, PlazaModel } from '@/api/modelPlaza'
import type { MonitorMatrixResponse } from '@/api/channelMonitorV2'
import { getPlatformSpec, platformDisplayName } from '@/constants/platformCatalog'
import { useAppStore } from '@/stores/app'
import { billingMode, catalogue, displayPrice, formatPrice, monitorKey, priceFields, pricingRows, primaryPrice, TOKEN_UNIT, USD_TO_CNY, type PlazaCurrency, type PlazaEntry, type PlazaOffer } from './plazaPricing'
const props = defineProps<{ response: ModelPlazaResponse | null; loading: boolean; error?: boolean; updatedAt?: Date | null; monitor?: MonitorMatrixResponse | null; monitorUnavailable?: boolean }>()
defineEmits<{ retry: [] }>()
const { t } = useI18n()
const app = useAppStore()
const search = ref('')
const selectedGroup = ref<number | 'all'>('all')
const filters = reactive({ platform: '', capability: '', billing: '', protocol: '' })
const filtersOpen = ref(false)
const currency = ref<PlazaCurrency>('CNY')
const unit = TOKEN_UNIT
const showOriginal = ref(false)
const sort = ref('name')
const view = ref('grid')
const chosenGroups = reactive<Record<string, number>>({})
const detailKey = ref('')
const detailTab = ref<'pricing' | 'status'>('pricing')
const allEntries = computed(() => catalogue(props.response?.groups ?? []))
const visibleGroups = computed(() => (props.response?.groups ?? []).filter(group =>
  !filters.platform || group.models.some(model => model.platform === filters.platform)))
watch(visibleGroups, groups => {
  if (selectedGroup.value !== 'all' && !groups.some(group => group.id === selectedGroup.value)) selectedGroup.value = 'all'
})
const descriptionHtml = computed(() => DOMPurify.sanitize(marked.parse(props.response?.description ?? '') as string))
const unitLabel = '1M Tokens'
const priceTitle = computed(() => t(showOriginal.value ? 'modelPlaza.catalog.referenceTitle' : 'modelPlaza.catalog.currentPrice'))
const referenceHint = computed(() => t('modelPlaza.catalog.referenceHint', { fx: USD_TO_CNY.toFixed(2) }))
const conversionNote = computed(() => showOriginal.value ? referenceHint.value : props.response?.balance_recharge_multiplier
  ? t('modelPlaza.catalog.conversion', { rate: props.response.balance_recharge_multiplier, fx: USD_TO_CNY.toFixed(2) })
  : t('modelPlaza.catalog.conversionUnavailable'))
const monitorRows = computed(() => new Map((props.monitor?.items ?? []).map(row => [monitorKey(row.platform, row.group_id, row.model), row])))
function status(item: PlazaOffer) { return monitorRows.value.get(monitorKey(item.model.platform, item.group.id, item.model.name)) }
function protocol(m: PlazaModel) {
  const gateway = getPlatformSpec(m.platform)?.gateway
  if (!gateway) return '—'
  return ({ anthropic: 'Messages', openai: 'OpenAI', gemini: 'Gemini', antigravity: 'Gemini' })[gateway]
}
function capabilities(m: PlazaModel) {
  const values = [billingMode(m) === 'image' ? 'image' : billingMode(m) === 'video' ? 'video' : 'text']
  if (m.pricing?.cache_read_price != null || m.pricing?.cache_write_price != null) values.push('cache')
  if (m.long_context_basis) values.push('context')
  return values
}
function firstTierLabel(m: PlazaModel) { const tier = pricingRows(m, showOriginal.value)[0]; return tier && 'tier_label' in tier ? tier.tier_label : '' }
function modeLabel(m: PlazaModel) { return t(`modelPlaza.catalog.mode_${billingMode(m)}`) }
function priceUnit(m: PlazaModel) { return billingMode(m) === 'token' ? `/ ${unitLabel}` : t(`modelPlaza.catalog.unit_${billingMode(m)}`) }
function offer(entry: PlazaEntry): PlazaOffer { return entry.offers.find(item => item.group.id === chosenGroups[entry.key]) ?? entry.offers[0] }
const filterSections = computed(() => {
  const models = allEntries.value.map(entry => entry.offers[0].model)
  const definitions = [
    { key: 'platform' as const, label: t('modelPlaza.catalog.provider'), values: (m: PlazaModel) => [m.platform], labelFor: platformDisplayName },
    { key: 'capability' as const, label: t('modelPlaza.catalog.capabilities'), values: capabilities, labelFor: (v: string) => t(`modelPlaza.catalog.cap_${v}`) },
    { key: 'billing' as const, label: t('modelPlaza.catalog.billing'), values: (m: PlazaModel) => [billingMode(m)], labelFor: (v: string) => t(`modelPlaza.catalog.mode_${v}`) },
    { key: 'protocol' as const, label: t('modelPlaza.catalog.protocol'), values: (m: PlazaModel) => [protocol(m)], labelFor: (v: string) => v }
  ]
  return definitions.map(section => {
    const counts = new Map<string, number>()
    for (const model of models) for (const v of section.values(model)) counts.set(v, (counts.get(v) ?? 0) + 1)
    return { ...section, options: [...counts].map(([value, count]) => ({ value, count, label: section.labelFor(value) })) }
  })
})
const entries = computed(() => {
  const query = search.value.trim().toLowerCase()
  const result = allEntries.value.map(entry => ({ ...entry, offers: entry.offers.filter(item => {
    const m = item.model
    return (selectedGroup.value === 'all' || item.group.id === selectedGroup.value)
      && (!filters.platform || m.platform === filters.platform)
      && (!filters.capability || capabilities(m).includes(filters.capability))
      && (!filters.billing || billingMode(m) === filters.billing)
      && (!filters.protocol || protocol(m) === filters.protocol)
      && (!query || `${m.name} ${platformDisplayName(m.platform)} ${item.group.name}`.toLowerCase().includes(query))
  }) })).filter(entry => entry.offers.length)
  return result.sort((a, b) => {
    const left = offer(a), right = offer(b)
    if (sort.value === 'price') return billingMode(left.model).localeCompare(billingMode(right.model)) || (displayPrice(primaryPrice(left, showOriginal.value), left, currency.value, props.response?.balance_recharge_multiplier, showOriginal.value) ?? Infinity) - (displayPrice(primaryPrice(right, showOriginal.value), right, currency.value, props.response?.balance_recharge_multiplier, showOriginal.value) ?? Infinity)
    if (sort.value === 'health') return (status(right)?.metrics.success_rate ?? -1) - (status(left)?.metrics.success_rate ?? -1)
    return left.model.name.localeCompare(right.model.name)
  })
})
const detailEntry = computed(() => allEntries.value.find(entry => entry.key === detailKey.value))
const allDetailOffers = computed(() => detailEntry.value?.offers ?? [])
function formattedPrice(value: number | null | undefined, item: PlazaOffer, scale: number) { return formatPrice(displayPrice(value, item, currency.value, props.response?.balance_recharge_multiplier, showOriginal.value), scale, currency.value) }
function openDetail(key: string, tab: 'pricing' | 'status') {
  const entry = entries.value.find(item => item.key === key)
  if (entry) chosenGroups[key] = offer(entry).group.id
  detailKey.value = key
  detailTab.value = tab
}
function reset() { search.value = ''; selectedGroup.value = 'all'; filters.platform = ''; filters.capability = ''; filters.billing = ''; filters.protocol = '' }
async function copy(name: string) {
  try { await navigator.clipboard.writeText(name); app.showSuccess(t('common.copied')) }
  catch { app.showError(t('common.copyFailed')) }
}
</script>
