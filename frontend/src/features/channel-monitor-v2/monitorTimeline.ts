import type { MonitorCoverage } from '@/api/channelMonitorV2'

/** The selected time window, including gaps and history not aggregated yet. */
export function monitorBucketStarts(coverage: MonitorCoverage): string[] {
  const step = Math.max(60, coverage.bucket_seconds) * 1000
  const start = Date.parse(coverage.requested_start)
  const requestedEnd = Date.parse(coverage.requested_end || '')
  const end = Number.isFinite(requestedEnd) && requestedEnd > start
    ? requestedEnd
    : Date.parse(coverage.data_through)
  if (![start, end, step].every(Number.isFinite) || start >= end) return []
  const starts: string[] = []
  for (let cursor = Math.floor(start / step) * step; cursor < end; cursor += step) {
    starts.push(new Date(cursor).toISOString())
  }
  return starts
}
