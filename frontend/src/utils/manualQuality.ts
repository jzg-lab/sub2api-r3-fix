// Use only as srcdoc in an iframe with sandbox="allow-scripts", never with v-html.
export function qualityPreviewHTML(answer: string): string | null {
  const blocks = Array.from(answer.matchAll(/```(?:html|svg|xml)?\s*\n([\s\S]*?)```/gi))
  const source = blocks.find(block => /<svg\b/i.test(block[1]))?.[1] || answer
  if (!/<svg\b[\s\S]*<\/svg\s*>/i.test(source)) return null

  // Template contents stay inert while external documents and policy overrides are removed.
  const template = document.createElement('template')
  template.innerHTML = source
  if (!template.content.querySelector('svg')) return null
  for (const element of template.content.querySelectorAll('base, meta, link, iframe, frame, object, embed, script[src]')) element.remove()

  // srcdoc inherits the main page CSP; reuse its nonce without relaxing that policy.
  const nonce = document.querySelector<HTMLScriptElement>('script[nonce]')?.nonce || crypto.randomUUID()
  for (const element of template.content.querySelectorAll('script, style')) element.setAttribute('nonce', nonce)
  const policy = `default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; img-src data: blob:; base-uri 'none'; form-action 'none'`
  return `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${policy}"><meta name="viewport" content="width=device-width,initial-scale=1"><style nonce="${nonce}">html,body{margin:0;height:100%;}body{display:grid;place-items:center;}svg{display:block;max-width:100vw!important;max-height:100vh!important;height:auto;}</style></head><body>${template.innerHTML}</body></html>`
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
