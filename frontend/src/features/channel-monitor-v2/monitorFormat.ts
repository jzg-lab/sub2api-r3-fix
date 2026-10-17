/**
 * Shared display formatters for channel-monitor-v2.
 * Kept pure so unit tests can lock metric presentation accuracy.
 *
 * Privacy: prefer rates (error_rate, RPM, TPM, success %) over absolute
 * request/error/token counts in user-facing surfaces.
 */

import type { HealthScoreBand, MonitorHealth, MonitorMetric } from '@/api/channelMonitorV2'
import { formatCompactNumber } from '@/utils/format'

export function monitorIntlLocale(): string {
  if (typeof document !== 'undefined') {
    const htmlLang = document.documentElement.getAttribute('lang')?.trim()
    if (htmlLang) return htmlLang
  }
  if (typeof navigator !== 'undefined' && navigator.language) return navigator.language
  return 'zh-CN'
}

export function formatMonitorNumber(value: number, compactAt = 10000, locale = monitorIntlLocale()): string {
  return Intl.NumberFormat(locale, {
    notation: value >= compactAt ? 'compact' : 'standard',
    maximumFractionDigits: 1,
  }).format(value || 0)
}

export function formatMonitorRate(value: number, locale = monitorIntlLocale()): string {
  return Intl.NumberFormat(locale, { maximumFractionDigits: 1 }).format(value || 0)
}

/**
 * RPM/TPM throughput display: always K/M compact (1.0K, 1.5M) for ops-readable density.
 * Prefer this over formatMonitorRate for rate KPIs and table cells.
 */
export function formatMonitorThroughput(value: number | null | undefined): string {
  if (value == null || Number.isNaN(Number(value))) return '0'
  const n = Number(value)
  if (Math.abs(n) < 1000) {
    return Intl.NumberFormat(monitorIntlLocale(), { maximumFractionDigits: 1 }).format(n)
  }
  return formatCompactNumber(n)
}

/**
 * Backend stores tokens-per-minute as `tpm`. Convert to tokens-per-second for display.
 */
export function tokensPerSecondFromTpm(tpm: number | null | undefined): number {
  if (tpm == null || Number.isNaN(Number(tpm))) return 0
  return Number(tpm) / 60
}

/** Tokens/sec from backend TPM (per-minute), compact ops formatting. */
export function formatMonitorTokensPerSecond(tpm: number | null | undefined): string {
  return formatMonitorThroughput(tokensPerSecondFromTpm(tpm))
}


export function formatMonitorPercent(value: number, locale = monitorIntlLocale()): string {
  return `${new Intl.NumberFormat(locale, {
    minimumFractionDigits: value < 0.01 ? 2 : 1,
    maximumFractionDigits: value < 0.01 ? 2 : 1,
  }).format((value || 0) * 100)}%`
}

export function formatMonitorMs(value: number | null | undefined): string {
  if (value == null) return '-'
  return value >= 1000 ? `${(value / 1000).toFixed(1)}s` : `${Math.round(value)}ms`
}

export function formatMonitorSuccessRate(successRequests: number, requestCount: number): string {
  if (!requestCount) return '-'
  return formatMonitorPercent(successRequests / requestCount)
}

export function formatMonitorSuccessRateFromError(errorRate: number): string {
  return formatMonitorPercent(1 - (errorRate || 0))
}

/**
 * Map continuous 0–100 score to 11 fine bands for multi-stop green→yellow→red.
 * score10 = best (green), score0 = worst (red).
 */
export function scoreToBand(score: number | null | undefined): HealthScoreBand {
  if (score == null || Number.isNaN(score)) return 'unknown'
  const clamped = Math.max(0, Math.min(100, score))
  // 0–100 → score0..score10 (11 stops for green→yellow→red gradient)
  const band = Math.round(clamped / 10)
  return `score${Math.max(0, Math.min(10, band))}` as HealthScoreBand
}

export type HealthDisplayMode = 'overall' | 'success' | 'ttft' | 'cache'

/** Resolve the score used for a health mode. */
export function healthModeScore(
  health: MonitorHealth,
  mode: HealthDisplayMode,
): number | null {
  if (mode === 'success') {
    return health.error_rate_score ?? null
  }
  if (mode === 'ttft') {
    return health.ttft_score ?? null
  }
  if (mode === 'cache') {
    return health.cache_score ?? null
  }
  return health.score ?? null
}

export function healthScoreClass(
  health: MonitorHealth,
  mode: HealthDisplayMode,
  requestCount: number,
): string {
  const score = healthModeScore(health, mode)
  if (score == null) {
    if (requestCount <= 0) return 'health-unknown'
    // Fall back to coarse state when score is absent (older payloads).
    const coarse =
      mode === 'success'
        ? health.error_rate
        : mode === 'ttft'
          ? health.ttft
          : mode === 'cache'
            ? health.cache
            : health.overall
    return healthStateClass(coarse)
  }
  return `health-${scoreToBand(score)}`
}

export function healthStateClass(state: string | undefined): string {
  return `health-${state || 'unknown'}`
}

function monitorHasRequests(metrics: MonitorMetric, health: MonitorHealth): boolean {
  return metrics.request_count > 0 || health.error_rate !== 'unknown'
}

function monitorHasCache(metrics: MonitorMetric, health: MonitorHealth): boolean {
  return metrics.cache_rate_denominator > 0 || (health.cache != null && health.cache !== 'unknown')
}

export function monitorAvailabilityState(metrics: MonitorMetric, health: MonitorHealth): MonitorHealth['error_rate'] {
  if (!monitorHasRequests(metrics, health)) return 'unknown'
  if (health.error_rate !== 'unknown') return health.error_rate
  if (!Number.isFinite(metrics.error_rate)) return 'warning'

  const warning = health.thresholds?.warning_error_rate ?? 0.05
  const critical = health.thresholds?.critical_error_rate ?? 0.2
  return metrics.error_rate >= critical ? 'critical' : metrics.error_rate >= warning ? 'warning' : 'healthy'
}

function clampMonitorScore(score: number): number {
  return Math.max(0, Math.min(100, score))
}

function monitorTTFTScore(metrics: MonitorMetric, health: MonitorHealth): number | null {
  if (health.ttft_score == null && metrics.ttft.sample_count <= 0 && health.ttft === 'unknown') return null
  if (health.ttft_score != null && Number.isFinite(health.ttft_score)) {
    return clampMonitorScore(health.ttft_score)
  }
  const p50 = metrics.ttft.p50_ms
  const latency = p50 != null && Number.isFinite(p50) ? p50 : metrics.ttft.p95_ms
  if (latency == null || !Number.isFinite(latency)) return null

  const target = health.thresholds?.target_ttft_ms ?? 3000
  let critical = health.thresholds?.critical_ttft_ms ?? 10000
  if (critical <= target) critical = target * 2.4
  if (latency <= target) return 100
  if (latency >= critical) return 0
  return clampMonitorScore(100 * (1 - (latency - target) / (critical - target)))
}

/** Height score for the card timeline: cache 70%, first-token latency 30%. */
export function monitorPerformanceScore(metrics: MonitorMetric, health: MonitorHealth): number | null {
  if (!monitorHasRequests(metrics, health)) return null
  const scores: Array<[number, number]> = []
  if (monitorHasCache(metrics, health) && Number.isFinite(metrics.cache_rate)) {
    scores.push([clampMonitorScore(metrics.cache_rate * 100), 0.7])
  }
  const ttft = monitorTTFTScore(metrics, health)
  if (ttft != null) scores.push([ttft, 0.3])
  if (!scores.length) return null

  const weight = scores.reduce((sum, [, value]) => sum + value, 0)
  return scores.reduce((sum, [score, value]) => sum + score * value, 0) / weight
}

/** Privacy-safe latency summary: avg + p50 + p90 (no absolute sample counts). */
export function formatLatencyPrivacy(
  p50: number | null | undefined,
  p90: number | null | undefined,
  avg?: number | null | undefined,
  p95?: number | null | undefined,
): string {
  const parts: string[] = []
  if (avg != null) parts.push(`AVG ${formatMonitorMs(avg)}`)
  if (p50 != null) parts.push(`P50 ${formatMonitorMs(p50)}`)
  if (p90 != null) parts.push(`P90 ${formatMonitorMs(p90)}`)
  // p95 only as fallback when p90 missing (older payloads)
  if (p90 == null && p95 != null) parts.push(`P95 ${formatMonitorMs(p95)}`)
  return parts.length ? parts.join(' · ') : '-'
}

/**
 * KPI secondary line for latency: AVG + P90 only (P50 is the primary value).
 * Falls back to P95 when P90 is absent. Delimiter is " · " so MetricCell can
 * split into non-truncated chips.
 */
export function formatLatencyKpiSecondary(
  avg?: number | null | undefined,
  p90?: number | null | undefined,
  p95?: number | null | undefined,
): string {
  const parts: string[] = []
  if (avg != null && Number.isFinite(avg)) parts.push(`AVG ${formatMonitorMs(avg)}`)
  if (p90 != null && Number.isFinite(p90)) parts.push(`P90 ${formatMonitorMs(p90)}`)
  else if (p95 != null && Number.isFinite(p95)) parts.push(`P95 ${formatMonitorMs(p95)}`)
  return parts.length ? parts.join(' · ') : '-'
}

/** Fallback when i18n key is missing; prefer `channelMonitorV2.errorCategories.*`. */
export function monitorErrorCategoryLabel(category: string): string {
  return category
}

/** Public metrics redact counts. Health states retain whether samples are usable. */
export function monitorCardMetrics(metrics: MonitorMetric, health: MonitorHealth) {
  const hasRequests = monitorHasRequests(metrics, health)
  const hasCache = monitorHasCache(metrics, health)
  return {
    availability: hasRequests && Number.isFinite(metrics.error_rate)
      ? formatMonitorPercent(1 - metrics.error_rate) : '—',
    cache: hasCache && Number.isFinite(metrics.cache_rate) ? formatMonitorPercent(metrics.cache_rate) : '—',
    ttft: metrics.ttft.p50_ms != null && Number.isFinite(metrics.ttft.p50_ms)
      ? formatMonitorMs(metrics.ttft.p50_ms) : '—',
  }
}
