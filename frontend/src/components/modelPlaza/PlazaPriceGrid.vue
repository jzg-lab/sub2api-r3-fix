<template>
  <div class="plaza-price-grid" :class="{ 'per-request': !tokenMode }">
    <div v-for="field in fields" :key="field" class="plaza-price-cell">
      <span>{{ t(`modelPlaza.catalog.${field}`) }}</span>
      <strong>{{ formatPrice(displayPrice(prices?.[field], offer, currency, conversion, showOriginal), tokenMode ? TOKEN_UNIT : 1, currency) }}</strong>
    </div>
  </div>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { billingMode, displayPrice, formatPrice, priceFields, pricingRows, TOKEN_UNIT, type PlazaCurrency, type PlazaOffer } from './plazaPricing'
const props = defineProps<{ offer: PlazaOffer; currency: PlazaCurrency; conversion?: number; showOriginal: boolean }>()
const { t } = useI18n()
const tokenMode = computed(() => billingMode(props.offer.model) === 'token')
const fields = computed(() => tokenMode.value ? priceFields : ['per_request_price'] as const)
const prices = computed(() => pricingRows(props.offer.model, props.showOriginal)[0])
</script>
