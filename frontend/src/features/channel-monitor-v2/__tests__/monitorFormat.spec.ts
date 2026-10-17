import { describe, expect, it } from 'vitest'
import {
  formatLatencyKpiSecondary,
  formatLatencyPrivacy,
  formatMonitorMs,
  formatMonitorNumber,
  formatMonitorPercent,
  formatMonitorRate,
  formatMonitorSuccessRate,
  formatMonitorSuccessRateFromError,
  formatMonitorThroughput,
  formatMonitorTokensPerSecond,
  healthScoreClass,
  healthStateClass,
  monitorAvailabilityState,
  monitorPerformanceScore,
  scoreToBand,
  tokensPerSecondFromTpm,
} from '../monitorFormat'
import type { MonitorHealth } from '@/api/channelMonitorV2'

describe('monitorFormat accuracy', () => {
  it('converts backend TPM (per minute) to tokens/sec for display', () => {
    expect(tokensPerSecondFromTpm(60)).toBe(1)
    expect(tokensPerSecondFromTpm(6000)).toBe(100)
    expect(formatMonitorTokensPerSecond(60)).toBe('1')
    expect(formatMonitorTokensPerSecond(null)).toBe('0')
  })

  it('formats request/token counts with compact notation above threshold', () => {
    expect(formatMonitorNumber(10)).toBe('10')
    expect(formatMonitorNumber(9999)).toBe('9,999')
    // compact at >= 10000
    expect(formatMonitorNumber(10000)).toMatch(/1/)
    expect(formatMonitorNumber(0)).toBe('0')
  })

  it('formats rates and percents from raw API fractions without hardcoding pretty values', () => {
    expect(formatMonitorRate(12.56)).toBe('12.6')
    expect(formatMonitorPercent(0.1)).toBe('10.0%')
    expect(formatMonitorPercent(0.005)).toBe('0.50%')
    // value < 0.01 uses 2 decimal places (includes exact 0)
    expect(formatMonitorPercent(0)).toBe('0.00%')
    expect(formatMonitorPercent(0.5)).toBe('50.0%')
  })

  it('formats RPM/TPM throughput with K/M compact notation', () => {
    expect(formatMonitorThroughput(12.56)).toBe('12.6')
    expect(formatMonitorThroughput(999)).toMatch(/999/)
    expect(formatMonitorThroughput(1000)).toBe('1.0K')
    expect(formatMonitorThroughput(1500)).toBe('1.5K')
    expect(formatMonitorThroughput(1_000_000)).toBe('1.0M')
    expect(formatMonitorThroughput(1_500_000)).toBe('1.5M')
    expect(formatMonitorThroughput(0)).toBe('0')
  })

  it('formats latency ms/s boundaries from API fields', () => {
    expect(formatMonitorMs(null)).toBe('-')
    expect(formatMonitorMs(300)).toBe('300ms')
    expect(formatMonitorMs(1500)).toBe('1.5s')
    expect(formatMonitorMs(999)).toBe('999ms')
  })

  it('formats KPI secondary latency as AVG · P90 (P50 is primary)', () => {
    expect(formatLatencyKpiSecondary(150, 400)).toBe('AVG 150ms · P90 400ms')
    expect(formatLatencyKpiSecondary(1500, null, 2000)).toBe('AVG 1.5s · P95 2.0s')
    expect(formatLatencyKpiSecondary(null, null)).toBe('-')
  })

  it('computes success rate from success_requests / request_count', () => {
    expect(formatMonitorSuccessRate(9, 10)).toBe('90.0%')
    expect(formatMonitorSuccessRate(0, 0)).toBe('-')
    expect(formatMonitorSuccessRate(1, 3)).toBe('33.3%')
  })

  it('derives success rate from error_rate without absolute counts', () => {
    expect(formatMonitorSuccessRateFromError(0.1)).toBe('90.0%')
    expect(formatMonitorSuccessRateFromError(0)).toBe('100.0%')
  })

  it('maps continuous scores to multi-stop bands', () => {
    expect(scoreToBand(100)).toBe('score10')
    expect(scoreToBand(95)).toBe('score10')
    expect(scoreToBand(80)).toBe('score8')
    expect(scoreToBand(50)).toBe('score5')
    expect(scoreToBand(0)).toBe('score0')
    expect(scoreToBand(null)).toBe('unknown')
  })

  it('builds score-based health classes for matrix cells', () => {
    const health: MonitorHealth = {
      overall: 'warning',
      error_rate: 'warning',
      ttft: 'healthy',
      cache: 'warning',
      score: 52,
      error_rate_score: 40,
      ttft_score: 100,
      cache_score: 50,
      minimum_sample: 20,
    }
    expect(healthScoreClass(health, 'overall', 10)).toBe('health-score5')
    expect(healthScoreClass(health, 'success', 10)).toBe('health-score4')
    expect(healthScoreClass(health, 'ttft', 10)).toBe('health-score10')
    expect(healthScoreClass(health, 'cache', 10)).toBe('health-score5')
    // Redacted user payloads have request_count=0 but keep score fields.
    expect(healthScoreClass(health, 'overall', 0)).toBe('health-score5')
    expect(healthScoreClass({ ...health, score: null }, 'overall', 0)).toBe('health-unknown')
  })

  it('maps health states for status dots', () => {
    expect(healthStateClass('healthy')).toBe('health-healthy')
    expect(healthStateClass(undefined)).toBe('health-unknown')
  })

  it('uses availability thresholds for timeline color whenever requests exist', () => {
    const health: MonitorHealth = {
      overall: 'healthy', error_rate: 'unknown', ttft: 'unknown', minimum_sample: 0,
      thresholds: {
        warning_error_rate: 0.05, critical_error_rate: 0.2, target_ttft_ms: 3000,
        warning_ttft_ms: 3000, critical_ttft_ms: 10000, error_weight: 0.6, ttft_weight: 0.2,
      },
    }
    const metrics = { ...({ error_rate: 0.049, request_count: 1 } as MonitorMetric) }
    expect(monitorAvailabilityState(metrics, health)).toBe('healthy')
    expect(monitorAvailabilityState({ ...metrics, error_rate: 0.05 }, health)).toBe('warning')
    expect(monitorAvailabilityState({ ...metrics, error_rate: 0.2 }, health)).toBe('critical')
    expect(monitorAvailabilityState({ ...metrics, request_count: 0 }, health)).toBe('unknown')
  })

  it('weights cache 70% and TTFT 30% for timeline height', () => {
    const health: MonitorHealth = {
      overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy',
      score: 100, ttft_score: 100, minimum_sample: 0,
    }
    const metrics = {
      request_count: 1, cache_rate_denominator: 1, cache_rate: 0.5,
      ttft: { p50_ms: 3000 },
    } as MonitorMetric
    expect(monitorPerformanceScore(metrics, health)).toBe(65)
    expect(monitorPerformanceScore({ ...metrics, cache_rate_denominator: 0 }, { ...health, cache: 'unknown' })).toBe(100)
    expect(monitorPerformanceScore({ ...metrics, ttft: { p50_ms: null } }, { ...health, ttft_score: null, cache: 'healthy' })).toBe(50)
    expect(monitorPerformanceScore({ ...metrics, cache_rate_denominator: 0, ttft: { p50_ms: null } }, { ...health, cache: 'unknown', ttft_score: null })).toBeNull()
    expect(monitorPerformanceScore({ ...metrics, request_count: 0 }, { ...health, error_rate: 'unknown', cache: 'unknown', ttft_score: null })).toBeNull()
    expect(monitorPerformanceScore({ ...metrics, ttft: { p50_ms: 500, sample_count: 0 } }, { ...health, ttft: 'unknown', ttft_score: null })).toBe(50)
    expect(monitorPerformanceScore({ ...metrics, cache_rate_denominator: 0, ttft: { p50_ms: 6500, p95_ms: 8000 } }, {
      ...health, cache: 'unknown', ttft_score: null,
      thresholds: { warning_error_rate: 0.05, critical_error_rate: 0.2, target_ttft_ms: 3000, warning_ttft_ms: 3000, critical_ttft_ms: 10000, error_weight: 0.6, ttft_weight: 0.2 },
    })).toBeCloseTo(50)
  })

  it('formats privacy-safe latency lines with avg/p50/p90', () => {
    expect(formatLatencyPrivacy(100, 250, 120, 300)).toBe('AVG 120ms · P50 100ms · P90 250ms')
    expect(formatLatencyPrivacy(100, null, null, 300)).toBe('P50 100ms · P95 300ms')
    expect(formatLatencyPrivacy(null, null)).toBe('-')
  })
})
