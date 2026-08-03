// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers
//
// Plan 18 B2 — the fragment token bootstrap.
//
// The pairing URL is `https://<host>:<port>/#t=<token>`. A fragment is NEVER
// sent to the server, so the token cannot reach an access log or a proxy. This
// module is the whole of the client's side of that: read `location.hash`,
// exchange it for an HttpOnly cookie, and `history.replaceState` the fragment
// away so it does not sit in browser history or leak via `Referer` on the first
// external link a transcript renders.

import { exchangeToken } from './client.js'

// base64url, 256 bits → 43 chars. Matched strictly rather than loosely: a
// fragment that is not a token must not be shipped to the server "just in case".
const TOKEN_RE = /^[A-Za-z0-9_-]{20,512}$/

// The fragment as it was when the page loaded.
//
// `bootstrapAuth` runs in `onMounted`, by which time vue-router has already
// performed its initial navigation and rewritten history at least once. Reading
// the hash then works today, but it makes the token's survival depend on a
// third party's behaviour. `captureHash()` is therefore called as the FIRST
// statement of `main.js`, before the router exists, and the captured value is
// the fallback. Belt and braces on the one string in this app that must not be
// lost or leaked.
let capturedHash = ''

/** Snapshot `location.hash`. Call before anything can rewrite history. */
export function captureHash(win = globalThis.window) {
  capturedHash = win?.location?.hash ?? ''
  return capturedHash
}

/** Read and clear the snapshot — it is single-use, like the token in it. */
function takeCapturedHash() {
  const h = capturedHash
  capturedHash = ''
  return h
}

/**
 * Pull the pairing token out of `location.hash`, if there is one.
 * @returns {string|null}
 */
export function readTokenFromHash(hash) {
  const raw = typeof hash === 'string' ? hash : ''
  if (!raw.startsWith('#')) return null
  // Accept `#t=…` and `#…&t=…`; `URLSearchParams` handles both once the `#`
  // is dropped. A fragment we do not recognise yields null and is left alone.
  const params = new URLSearchParams(raw.slice(1))
  const token = params.get('t')
  if (!token || !TOKEN_RE.test(token)) return null
  return token
}

/**
 * Erase the fragment from the address bar and from history, without adding a
 * history entry and without reloading.
 */
export function stripHash(win = globalThis.window) {
  try {
    const { pathname, search } = win.location
    win.history.replaceState(null, '', `${pathname}${search}`)
  } catch {
    // Very old/locked-down browsers: at worst the fragment stays visible.
    // Not worth failing the boot over.
  }
}
/**
 * Boot-time token exchange.
 *
 * Returns `{ exchanged, device, error }`.
 * - `exchanged: false` with no error — there was no token in the fragment; the
 *   caller should just try the API and see whether an existing cookie works.
 * - `exchanged: true` — a fresh session cookie is set.
 * - `error` — an `ApiError` ('bad-token' / 'rate-limited' / 'network').
 *
 * The fragment is stripped in ALL cases, including failure. A token that was
 * rejected is still a secret that should not sit in history.
 */
export async function bootstrapAuth({ win = globalThis.window, fetchImpl } = {}) {
  const token = readTokenFromHash(win?.location?.hash) ?? readTokenFromHash(takeCapturedHash())
  if (!token) return { exchanged: false, device: null, error: null }

  try {
    const res = await exchangeToken(token, { fetchImpl })
    return { exchanged: true, device: res?.device ?? null, error: null }
  } catch (error) {
    return { exchanged: false, device: null, error }
  } finally {
    stripHash(win)
  }
}
