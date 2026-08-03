// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// The sanitizer corpus (plan 18 B3.5).
//
// **Assertions are on the PARSED DOM, never on substrings.** Spike 4 established
// why, and it is not a style preference:
//
//     expect(html).not.toContain('javascript:')
//
// FAILS against CORRECT output. `markdown-it` with `html:false` ESCAPES raw HTML
// rather than stripping it, and its link validator refuses `javascript:` URLs by
// leaving them as literal text — so the string is still present in the output,
// inert, as `&lt;script&gt;` or as plain text. A substring assertion would
// therefore reject a safe renderer and, worse, could be "fixed" by making the
// renderer delete text instead of escaping it.
//
// What actually matters is what the BROWSER builds. So every case parses the
// output and asserts on the resulting tree:
//
//   * no live `<script>` element
//   * no attribute whose name starts with `on`
//   * no `a[href]` starting with `javascript:` (or any non-http(s)/mailto scheme)
//   * no `iframe` / `object` / `embed` / `img`
//
// …and, just as importantly, that real markdown and highlighted code still
// render. A sanitizer that outputs nothing passes every negative test.

import { beforeEach, describe, expect, it } from 'vitest'
import { renderMarkdown, renderMarkdownInline, sanitizeHtml } from './render.js'

/** Parse into a real DOM and hand back the container. */
function parse(html) {
  const el = document.createElement('div')
  el.innerHTML = html
  return el
}

/** Every assertion that must hold for ANY rendered agent output. */
function expectInert(html) {
  const dom = parse(html)

  // 1. No live script element.
  expect(dom.querySelectorAll('script').length).toBe(0)
  expect(dom.querySelectorAll('noscript').length).toBe(0)

  // 2. No event-handler attribute anywhere in the tree.
  const withHandlers = []
  for (const node of dom.querySelectorAll('*')) {
    for (const attr of node.attributes) {
      if (attr.name.toLowerCase().startsWith('on')) {
        withHandlers.push(`${node.nodeName}[${attr.name}]`)
      }
    }
  }
  expect(withHandlers).toEqual([])

  // 3. No dangerous URL scheme survives on a link.
  for (const a of dom.querySelectorAll('a[href]')) {
    const href = a.getAttribute('href').trim().toLowerCase()
    expect(href.startsWith('javascript:')).toBe(false)
    expect(href.startsWith('data:')).toBe(false)
    expect(href.startsWith('vbscript:')).toBe(false)
    expect(/^(?:https?|mailto):/.test(href)).toBe(true)
  }

  // 4. No embedding or resource-loading element. `img` is included: a remote
  //    image is a third-party network call from a security tool, which the
  //    plan's conventions forbid outright.
  for (const tag of ['iframe', 'object', 'embed', 'img', 'svg', 'math', 'form', 'input', 'style', 'link', 'meta', 'base', 'video', 'audio', 'source']) {
    expect(dom.querySelectorAll(tag).length, `${tag} survived`).toBe(0)
  }

  // 5. No inline style — the one attribute that can reposition an element over
  //    the approve button.
  expect(dom.querySelectorAll('[style]').length).toBe(0)
}

describe('renderMarkdown — the sanitizer corpus', () => {
  const corpus = [
    ['a bare script tag', '<script>alert(1)</script>'],
    ['a script tag inside a paragraph', 'hello <script>alert(document.cookie)</script> world'],
    ['an img with onerror', '<img src=x onerror=alert(1)>'],
    ['an svg onload', '<svg/onload=alert(1)>'],
    ['a javascript: markdown link', '[click me](javascript:alert(1))'],
    ['a JavaScript: link with odd casing', '[click me](JaVaScRiPt:alert(1))'],
    ['a javascript link with an entity', '[x](java&#115;cript:alert(1))'],
    ['a data: URL link', '[x](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)'],
    ['a vbscript: link', '[x](vbscript:msgbox(1))'],
    ['a raw anchor with a handler', '<a href="#" onclick="alert(1)">x</a>'],
    ['an iframe', '<iframe src="https://evil.example/"></iframe>'],
    ['an object', '<object data="evil.swf"></object>'],
    ['an embed', '<embed src="evil.swf">'],
    ['a form with a formaction', '<form action="https://evil.example"><button formaction="https://evil.example">go</button></form>'],
    ['a style block', '<style>body{display:none}</style>'],
    ['a base tag', '<base href="https://evil.example/">'],
    ['a meta refresh', '<meta http-equiv="refresh" content="0;url=https://evil.example">'],
    ['a link rel=import', '<link rel="import" href="https://evil.example">'],
    ['a mutation-XSS shaped payload', '<noscript><p title="</noscript><img src=x onerror=alert(1)>">'],
    ['an inline style attribute', '<p style="position:fixed;inset:0">covered</p>'],
    ['a borrowed utility class', '<div class="fixed inset-0 z-50 bg-base-100">clickjack</div>'],
    ['an onerror inside a code fence', '```html\n<img src=x onerror=alert(1)>\n```'],
    ['a markdown image pointing at a tracker', '![](https://tracker.example/pixel.png)'],
    ['an autolinked javascript scheme', 'javascript:alert(1)'],
    ['nested markup', '<div><span><script>alert(1)</script></span></div>'],
    ['an unclosed script', '<script>alert(1)'],
    ['a srcset', '<img srcset="x 1x" src="y">'],
    ['a details/summary with a handler', '<details ontoggle=alert(1)><summary>x</summary>y</details>'],
    ['an anchor with a ping', '<a href="https://ok.example" ping="https://evil.example">x</a>'],
    ['a protocol-relative link', '[x](//evil.example/path)'],
  ]

  for (const [name, source] of corpus) {
    it(`renders ${name} inert`, () => {
      expectInert(renderMarkdown(source))
    })

    it(`renders ${name} inert inline`, () => {
      expectInert(renderMarkdownInline(source))
    })
  }

  it('never yields a live handler even when the payload is repeated many times', () => {
    const source = Array.from({ length: 50 }, (_, i) => `<img src=x${i} onerror=alert(${i})>`).join('\n\n')
    expectInert(renderMarkdown(source))
  })
})
describe('renderMarkdown — real content still renders', () => {
  it('renders headings, emphasis and lists', () => {
    const dom = parse(renderMarkdown('# Title\n\nSome **bold** and *italic*.\n\n- one\n- two\n'))
    expect(dom.querySelector('h1')?.textContent).toBe('Title')
    expect(dom.querySelector('strong')?.textContent).toBe('bold')
    expect(dom.querySelector('em')?.textContent).toBe('italic')
    expect(dom.querySelectorAll('li').length).toBe(2)
  })

  it('keeps safe links, and hardens them', () => {
    const dom = parse(renderMarkdown('[docs](https://example.com/x)'))
    const a = dom.querySelector('a')
    expect(a).toBeTruthy()
    expect(a.getAttribute('href')).toBe('https://example.com/x')
    expect(a.getAttribute('rel')).toContain('noopener')
    expect(a.getAttribute('rel')).toContain('noreferrer')
    expect(a.getAttribute('target')).toBe('_blank')
  })

  it('keeps mailto links', () => {
    const dom = parse(renderMarkdown('[mail](mailto:someone@example.com)'))
    expect(dom.querySelector('a')?.getAttribute('href')).toBe('mailto:someone@example.com')
  })

  it('renders inline code without turning it into markup', () => {
    const dom = parse(renderMarkdown('use `<script>` carefully'))
    const code = dom.querySelector('code')
    expect(code?.textContent).toBe('<script>')
    // The text is `<script>`; there must be no SCRIPT ELEMENT.
    expect(dom.querySelectorAll('script').length).toBe(0)
  })

  it('renders tables', () => {
    const dom = parse(renderMarkdown('| a | b |\n|---|---|\n| 1 | 2 |\n'))
    expect(dom.querySelectorAll('th').length).toBe(2)
    expect(dom.querySelectorAll('td').length).toBe(2)
  })

  it('renders blockquotes and horizontal rules', () => {
    const dom = parse(renderMarkdown('> quoted\n\n---\n'))
    expect(dom.querySelector('blockquote')).toBeTruthy()
    expect(dom.querySelector('hr')).toBeTruthy()
  })

  it('keeps an image alt as inert text rather than loading anything', () => {
    const dom = parse(renderMarkdown('![a diagram](https://tracker.example/p.png)'))
    expect(dom.querySelectorAll('img').length).toBe(0)
    expect(dom.textContent).toContain('a diagram')
  })

  it('preserves the newline-is-a-break convention agents write in', () => {
    const dom = parse(renderMarkdown('line one\nline two'))
    expect(dom.querySelectorAll('br').length).toBe(1)
  })
})

describe('renderMarkdown — highlighted code', () => {
  it('highlights a known language and keeps only hljs classes', () => {
    const dom = parse(renderMarkdown('```js\nconst x = 1\n```'))
    const pre = dom.querySelector('pre')
    expect(pre).toBeTruthy()
    expect(pre.className).toContain('hljs')
    // Real highlighting happened: hljs emits spans.
    const spans = dom.querySelectorAll('span')
    expect(spans.length).toBeGreaterThan(0)
    for (const s of spans) {
      for (const cls of s.className.split(/\s+/).filter(Boolean)) {
        expect(cls.startsWith('hljs')).toBe(true)
      }
    }
    expect(dom.textContent).toContain('const')
  })

  it('highlights each language in the pinned subset without throwing', () => {
    for (const lang of ['bash', 'go', 'python', 'json', 'yaml', 'diff', 'cpp', 'cmake']) {
      const html = renderMarkdown('```' + lang + '\nx\n```')
      expect(html).not.toBe('')
      expectInert(html)
    }
  })

  it('falls back to escaped text for an unknown language', () => {
    const dom = parse(renderMarkdown('```notalanguage\n<script>alert(1)</script>\n```'))
    expect(dom.querySelectorAll('script').length).toBe(0)
    expect(dom.querySelector('code')?.textContent).toContain('<script>')
  })

  it('does not let a fence info string smuggle a class in', () => {
    const dom = parse(renderMarkdown('```fixed inset-0\nx\n```'))
    for (const node of dom.querySelectorAll('*')) {
      for (const cls of (node.className || '').split(/\s+/).filter(Boolean)) {
        expect(/^(?:hljs|hljs-[\w-]+|language-[\w-]+|ak-md-img)$/.test(cls)).toBe(true)
      }
    }
  })
})

describe('sanitizeHtml — the barrier itself', () => {
  beforeEach(() => {
    // Hooks are process-global; make sure a prior test cannot have removed them.
    sanitizeHtml('<p>warm up</p>')
  })

  it('strips a script element but keeps surrounding text', () => {
    const dom = parse(sanitizeHtml('<p>before<script>alert(1)</script>after</p>'))
    expect(dom.querySelectorAll('script').length).toBe(0)
    expect(dom.textContent).toContain('before')
    expect(dom.textContent).toContain('after')
  })

  it('drops a javascript: href but keeps the link text', () => {
    const dom = parse(sanitizeHtml('<a href="javascript:alert(1)">press</a>'))
    expect(dom.querySelectorAll('a[href]').length).toBe(0)
    expect(dom.textContent).toContain('press')
  })

  it('handles empty and non-string input', () => {
    expect(sanitizeHtml('')).toBe('')
    expect(sanitizeHtml(null)).toBe('')
    expect(sanitizeHtml(undefined)).toBe('')
  })

  it('returns empty for empty markdown', () => {
    expect(renderMarkdown('')).toBe('')
    expect(renderMarkdown(null)).toBe('')
    expect(renderMarkdown(undefined)).toBe('')
  })

  it('truncates a hostile-sized document rather than parsing it forever', () => {
    const html = renderMarkdown('a'.repeat(300 * 1024))
    expect(html).toContain('truncated by the phone')
    expectInert(html)
  })
})
