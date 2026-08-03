// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// Plan 18 B3.5 — untrusted content.
//
// Transcripts render agent output, and agent output is attacker-influenceable —
// precisely the adversary `docs/security-model.md` exists for. In a browser
// holding a session cookie, ONE XSS is full remote control of every agent. This
// module is the only place in the app that produces HTML, and
// `components/MarkdownBlock.vue` is the only place that consumes it. Nothing
// else may reach `v-html`.
//
// The pipeline is `markdown-it (html:false)` → `DOMPurify` → only then `v-html`.
// Both stages are kept, deliberately:
//
//   * `markdown-it` with `html:false` ESCAPES raw HTML rather than stripping it,
//     and its own link validator already refuses `javascript:` URLs — so
//     `[a](javascript:alert(1))` never becomes an anchor at all.
//   * DOMPurify is therefore defence in depth for the markdown path — and it
//     becomes LOAD-BEARING the moment `highlight.js` output is injected raw
//     through the `highlight` callback, which it must be, because that is the
//     one place a string bypasses markdown-it's escaping.
//
// Three narrowings beyond the plan's sketch, each for a concrete reason:
//
//   * **No `<img>`, ever.** `![](https://tracker.example/x.png)` is a
//     third-party network call issued by a security tool from inside the user's
//     LAN — banned by the plan's conventions ("no CDNs, no telemetry, no
//     third-party network calls"). Images render as an inert text placeholder.
//   * **`class` is filtered to an allowlist.** Tailwind is JIT-scanned, so any
//     utility used anywhere in this app exists in the stylesheet; an attacker
//     who could set `class="fixed inset-0 …"` could paint over the approve
//     button. Only `hljs*` and `language-*` survive.
//   * **URLs are restricted to http/https/mailto.** No `data:`, no `blob:`.

import MarkdownIt from 'markdown-it'
import DOMPurify from 'dompurify'
import hljs from 'highlight.js/lib/core'

// An EXPLICIT language subset from `highlight.js/lib/core` — never the full
// bundle, which is megabytes and would be shipped over mobile data to
// syntax-colour a language nobody in this tree uses. Aliases (`sh`, `js`, `py`,
// `yml`, `html`, `toml`, …) come along with each grammar automatically.
//
// The subset is chosen by measured cost against actual value: all 22 candidates
// came to ~57 kB gzip, these fourteen to ~25 kB. What is left out (typescript
// 6.7 kB, css 5.4, sql 3.7, ruby 3.4, c 3.1, java 2.1, makefile 1.0,
// dockerfile 0.6) falls back to ESCAPED PLAIN TEXT — correct and safe, just not
// coloured. Adding one back is a one-line import and a measurable decision.
import bash from 'highlight.js/lib/languages/bash'
import cmake from 'highlight.js/lib/languages/cmake'
import cpp from 'highlight.js/lib/languages/cpp'
import diff from 'highlight.js/lib/languages/diff'
import go from 'highlight.js/lib/languages/go'
import ini from 'highlight.js/lib/languages/ini'
import javascript from 'highlight.js/lib/languages/javascript'
import json from 'highlight.js/lib/languages/json'
import markdown from 'highlight.js/lib/languages/markdown'
import plaintext from 'highlight.js/lib/languages/plaintext'
import python from 'highlight.js/lib/languages/python'
import xml from 'highlight.js/lib/languages/xml'
import yaml from 'highlight.js/lib/languages/yaml'

const LANGUAGES = {
  bash,
  cmake,
  cpp,
  diff,
  go,
  ini,
  javascript,
  json,
  markdown,
  plaintext,
  python,
  xml,
  yaml,
}

for (const [name, def] of Object.entries(LANGUAGES)) {
  hljs.registerLanguage(name, def)
}

export const SUPPORTED_LANGUAGES = Object.freeze(Object.keys(LANGUAGES))

// A single oversized message must not lock up a phone's main thread doing
// markdown parsing. The server already caps its responses (M0.4); this is the
// client-side backstop, and it is honest about having cut.
const MAX_SOURCE_BYTES = 256 * 1024

const md = new MarkdownIt({
  html: false, // escape raw HTML rather than passing it through
  xhtmlOut: false,
  breaks: true, // agents write chat-shaped prose; a lone newline means one
  linkify: true,
  typographer: false,
  highlight(str, lang) {
    const language = lang ? String(lang).trim().toLowerCase() : ''
    if (language && hljs.getLanguage(language)) {
      try {
        const out = hljs.highlight(str, { language, ignoreIllegals: true }).value
        // RAW HTML, injected verbatim by markdown-it. This is the one string in
        // the pipeline that markdown-it does not escape, and the reason
        // DOMPurify is not optional.
        return `<pre class="hljs"><code class="language-${escapeAttr(language)}">${out}</code></pre>`
      } catch {
        /* fall through to the escaped form */
      }
    }
    return `<pre class="hljs"><code>${md.utils.escapeHtml(str)}</code></pre>`
  },
})

// Images are not rendered. Replacing the renderer rule (rather than relying on
// DOMPurify to drop the tag) keeps the alt text, so the reader still sees that
// something was there and what it was called.
md.renderer.rules.image = (tokens, idx) => {
  const token = tokens[idx]
  const alt = token.content || token.attrGet('alt') || 'image'
  return `<span class="ak-md-img">[image: ${md.utils.escapeHtml(alt)}]</span>`
}

function escapeAttr(s) {
  return String(s).replace(/[^a-z0-9_-]/gi, '')
}

const ALLOWED_TAGS = [
  'p', 'br', 'hr',
  'h1', 'h2', 'h3', 'h4', 'h5', 'h6',
  'strong', 'em', 'b', 'i', 'u', 's', 'del', 'ins', 'mark', 'sub', 'sup', 'small',
  'blockquote', 'ul', 'ol', 'li', 'dl', 'dt', 'dd',
  'code', 'pre', 'kbd', 'samp', 'var', 'span',
  'a',
  'table', 'thead', 'tbody', 'tfoot', 'tr', 'th', 'td', 'caption',
]

const ALLOWED_ATTR = ['href', 'title', 'class', 'lang', 'dir', 'start', 'colspan', 'rowspan', 'align']

// http/https/mailto only. Everything else — `javascript:`, `data:`, `blob:`,
// `vbscript:`, protocol-relative `//host` — is dropped.
const ALLOWED_URI_REGEXP = /^(?:https?|mailto):/i

// Only classes this app's own renderers emit. Anything else is stripped, so an
// agent cannot borrow a Tailwind utility to paint over the UI.
const CLASS_ALLOW = /^(?:hljs|hljs-[a-z0-9_-]+|language-[a-z0-9_-]+|ak-md-img)$/i

let hooksInstalled = false

function installHooks() {
  if (hooksInstalled) return
  hooksInstalled = true

  DOMPurify.addHook('afterSanitizeAttributes', (node) => {
    if (!node || typeof node.getAttribute !== 'function') return

    // Class allowlist.
    if (node.hasAttribute?.('class')) {
      const kept = String(node.getAttribute('class'))
        .split(/\s+/)
        .filter((c) => c && CLASS_ALLOW.test(c))
      if (kept.length) node.setAttribute('class', kept.join(' '))
      else node.removeAttribute('class')
    }

    // Links leave the app: no opener, no referrer, and marked untrusted.
    // `Referrer-Policy: no-referrer` is set server-side too — this is the
    // per-element belt to that braces, and it is what stops a transcript link
    // from carrying the origin (and any surviving fragment) to a third party.
    if (node.nodeName === 'A') {
      const href = node.getAttribute('href')
      if (href && !ALLOWED_URI_REGEXP.test(href.trim())) {
        node.removeAttribute('href')
      }
      if (node.hasAttribute('href')) {
        node.setAttribute('target', '_blank')
        node.setAttribute('rel', 'noopener noreferrer nofollow ugc')
      }
    }
  })
}

/**
 * Sanitize a fragment of HTML.
 *
 * Exported so the test corpus can drive the barrier directly, and so any future
 * HTML producer has exactly one door to come through.
 */
export function sanitizeHtml(dirty) {
  installHooks()
  return DOMPurify.sanitize(String(dirty ?? ''), {
    ALLOWED_TAGS,
    ALLOWED_ATTR,
    ALLOWED_URI_REGEXP,
    // Belt and braces on top of the tag allowlist: named explicitly so that
    // widening ALLOWED_TAGS by accident cannot quietly re-admit them.
    FORBID_TAGS: [
      'script', 'style', 'iframe', 'object', 'embed', 'form', 'input', 'button',
      'textarea', 'select', 'option', 'link', 'meta', 'base', 'svg', 'math',
      'img', 'video', 'audio', 'source', 'track', 'template', 'noscript',
    ],
    FORBID_ATTR: ['style', 'srcset', 'src', 'formaction', 'action', 'xlink:href', 'ping'],
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false,
    ALLOW_UNKNOWN_PROTOCOLS: false,
    USE_PROFILES: { html: true },
    // Keep the text of a dropped element rather than deleting the content —
    // seeing an inert `alert(1)` as text is more honest than silence.
    KEEP_CONTENT: true,
    RETURN_DOM: false,
    RETURN_DOM_FRAGMENT: false,
    RETURN_TRUSTED_TYPE: false,
    SANITIZE_DOM: true,
  })
}

/**
 * Render untrusted markdown to HTML that is safe to `v-html`.
 *
 * This is the ONLY function whose output may be bound with `v-html`.
 *
 * @param {string} source
 * @returns {string}
 */
export function renderMarkdown(source) {
  const text = normaliseSource(source)
  if (!text) return ''
  let dirty
  try {
    dirty = md.render(text)
  } catch {
    // A parser failure must never fall back to raw text: that is precisely the
    // path that would put unescaped model output on the page.
    dirty = `<p>${md.utils.escapeHtml(text)}</p>`
  }
  return sanitizeHtml(dirty)
}

/** Render a single line (no block wrapper). Same barrier. */
export function renderMarkdownInline(source) {
  const text = normaliseSource(source)
  if (!text) return ''
  let dirty
  try {
    dirty = md.renderInline(text)
  } catch {
    dirty = md.utils.escapeHtml(text)
  }
  return sanitizeHtml(dirty)
}

function normaliseSource(source) {
  if (source === null || source === undefined) return ''
  let text = typeof source === 'string' ? source : String(source)
  if (text.length > MAX_SOURCE_BYTES) {
    text = `${text.slice(0, MAX_SOURCE_BYTES)}\n\n… (truncated by the phone — open this agent on the desktop for the full text)`
  }
  return text
}

/** Test seam: DOMPurify hooks are process-global, so tests can undo them. */
export function _resetHooksForTests() {
  DOMPurify.removeAllHooks()
  hooksInstalled = false
}
