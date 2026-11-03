import { describe, expect, it } from 'vitest'
import { qualityAnswerMatches, qualityPreviewHTML } from '../manualQuality'

describe('manual quality display', () => {
  it('does not mistake numbers in reasoning for the final answer', () => {
    expect(qualityAnswerMatches('21 is not correct.\n22', '21')).toBe(false)
    expect(qualityAnswerMatches('Reasoning\n答案：21。', '21')).toBe(true)
    expect(qualityAnswerMatches('21', '')).toBeNull()
  })
  it('preserves HTML styles, SVG animations and inline scripts without executing them in the app', () => {
    const preview = qualityPreviewHTML('Explanation\n```html\n<!doctype html><html><head><style>@keyframes spin { to { transform: rotate(360deg); } } .wheel { animation: spin 1s linear infinite; }</style></head><body><svg><g class="wheel"><circle r="10"/><animateTransform attributeName="transform" type="rotate" dur="1s" repeatCount="indefinite"/></g></svg><script>document.body.dataset.generated = "executed"; requestAnimationFrame(() => {});</script></body></html>\n```\nMore explanation')!
    expect(preview).toContain('@keyframes spin')
    expect(preview).toContain('animation: spin 1s linear infinite')
    expect(preview).toContain('<animateTransform')
    expect(preview).toContain('repeatCount="indefinite"')
    expect(preview).toContain('requestAnimationFrame')
    expect(preview).not.toContain('Explanation')
    expect(document.body.dataset.generated).toBeUndefined()
  })
  it('puts the restrictive policy first and removes generated policy, external scripts and nested frames', () => {
    const preview = qualityPreviewHTML('<meta http-equiv="Content-Security-Policy" content="script-src *"><base href="https://evil.invalid"><link rel="stylesheet" href="https://evil.invalid/a"><svg><circle r="10"/></svg><script src="https://evil.invalid/a.js"></script><iframe srcdoc="bad"></iframe><object data="bad"></object><embed src="bad">')!
    const doc = new DOMParser().parseFromString(preview, 'text/html')
    const policy = doc.querySelector('meta[http-equiv="Content-Security-Policy"]')!
    expect(policy.getAttribute('content')).toContain("default-src 'none'")
    expect(policy.getAttribute('content')).toContain("base-uri 'none'; form-action 'none'")
    expect(doc.querySelectorAll('meta[http-equiv]')).toHaveLength(1)
    expect(doc.querySelector('base, link, iframe, object, embed, script[src]')).toBeNull()
    expect(preview.indexOf('Content-Security-Policy')).toBeLessThan(preview.indexOf('<svg'))
    expect(preview).not.toContain('evil.invalid')
  })
  it('reuses the main page nonce so inherited production CSP permits inline animation scripts', () => {
    const main = document.createElement('script')
    main.nonce = 'production-csp-nonce'
    document.head.append(main)
    try {
      const preview = qualityPreviewHTML('<style>.wheel{opacity:1}</style><svg><circle r="10"/></svg><script>requestAnimationFrame(() => {})</script>')!
      expect(preview).toContain("script-src 'nonce-production-csp-nonce'")
      expect(preview).toContain('<script nonce="production-csp-nonce">')
      expect(preview).toContain('<style nonce="production-csp-nonce">')
    } finally { main.remove() }
  })
  it('rejects missing or truncated SVG', () => {
    expect(qualityPreviewHTML('<svg><circle/>')).toBeNull()
    expect(qualityPreviewHTML('no drawing')).toBeNull()
  })
})
