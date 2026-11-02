import { describe, expect, it } from 'vitest'
import { qualityAnswerMatches, qualitySVGDataURL } from '../manualQuality'

describe('manual quality display', () => {
  it('does not mistake numbers in reasoning for the final answer', () => {
    expect(qualityAnswerMatches('21 is not correct.\n22', '21')).toBe(false)
    expect(qualityAnswerMatches('Reasoning\n答案：21。', '21')).toBe(true)
    expect(qualityAnswerMatches('21', '')).toBeNull()
  })
  it('renders only static SVG and removes executable or remote content', () => {
    const url = qualitySVGDataURL('```svg\n<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(1)</script><foreignObject><div>bad</div></foreignObject><image href="https://evil.invalid/a"/><style>circle { fill:url(https://evil.invalid/b) }</style><circle r="10" fill="url(https://evil.invalid/c)"/><animate attributeName="href" values="https://evil.invalid"/></svg>\n```')
    expect(url).toMatch(/^data:image\/svg\+xml/)
    const text = decodeURIComponent(url!.split(',').slice(1).join(','))
    expect(text).toContain('<circle')
    expect(text).not.toMatch(/script|foreignObject|evil\.invalid|onload|<animate|<style|<image/i)
  })
  it('rejects missing or truncated SVG', () => {
    expect(qualitySVGDataURL('<svg><circle/>')).toBeNull()
    expect(qualitySVGDataURL('no drawing')).toBeNull()
  })
})
