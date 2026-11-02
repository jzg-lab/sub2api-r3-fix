import DOMPurify from 'dompurify'

// Render as an SVG image, never as executable HTML in the application document.
// Static geometry only: remove embedded documents, animation and resource URLs.
export function qualitySVGDataURL(answer: string): string | null {
  const start = answer.indexOf('<svg')
  const end = answer.lastIndexOf('</svg>')
  if (start < 0 || end < start) return null
  const sanitized = DOMPurify.sanitize(answer.slice(start, end + 6), {
    USE_PROFILES: { svg: true },
    FORBID_TAGS: ['script', 'foreignObject', 'style', 'image', 'use', 'a', 'animate', 'animateMotion', 'animateTransform', 'set'],
    FORBID_ATTR: ['href', 'xlink:href', 'style']
  })
  const doc = new DOMParser().parseFromString(sanitized, 'image/svg+xml')
  const svg = doc.documentElement
  if (svg.localName !== 'svg' || doc.querySelector('parsererror')) return null
  for (const element of [svg, ...Array.from(svg.querySelectorAll('*'))]) {
    for (const attr of Array.from(element.attributes)) {
      if (/url\s*\(/i.test(attr.value) && !/^url\(#[\w-]+\)$/.test(attr.value)) element.removeAttribute(attr.name)
    }
  }
  svg.setAttribute('xmlns', 'http://www.w3.org/2000/svg')
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(new XMLSerializer().serializeToString(svg))}`
}

// A conservative comparison hint, not a semantic grade. Never search the entire
// reasoning for a number ("21 is wrong, 22 is right" must not be called a match).
export function qualityAnswerMatches(answer: string, reference: string): boolean | null {
  if (!reference.trim()) return null
  const last = answer.trim().split('\n').filter(line => line.trim()).at(-1) || ''
  const normalize = (text: string) => text.trim().replace(/\*\*/g, '').replace(/[。.!！]$/, '').trim()
  const final = normalize(last).replace(/^(?:最终答案|答案|Answer|Final answer)\s*[:：]\s*/i, '')
  return normalize(reference) === final
}
