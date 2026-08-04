// The browser's only network door. Sending is deliberately one fixed operation:
// a paired device may only queue a human follow-up through the core-owned
// human-surface path; it cannot request an immediate/interleaved write.
export const API_BASE = '/api/v1'
export const API_VERSION = 1

export class ApiError extends Error {
  constructor(code, message, { status = 0, body = null } = {}) {
    super(message || code)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.body = body
  }

  get isAuth() { return this.code === 'unauthenticated' || this.status === 401 }
}

export async function apiFetch(path, opts = {}) {
  const { method = 'GET', body, query, signal, fetchImpl = globalThis.fetch?.bind(globalThis) } = opts
  if (typeof fetchImpl !== 'function') throw new ApiError('network', 'This browser cannot make requests.')
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value !== undefined && value !== null && value !== '') params.set(key, String(value))
  }
  const url = `${API_BASE}${path.startsWith('/') ? path : `/${path}`}${params.size ? `?${params}` : ''}`
  const headers = { Accept: 'application/json' }
  const init = { method, headers, signal, credentials: 'same-origin', cache: 'no-store', redirect: 'error', referrerPolicy: 'no-referrer' }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  let res
  try { res = await fetchImpl(url, init) } catch (err) {
    if (err?.name === 'AbortError') throw err
    throw new ApiError('network', 'Cannot reach Agent Kate. Check the connection.')
  }
  let payload = null
  try { payload = (await res.text()) || null; payload = payload ? JSON.parse(payload) : {} } catch { payload = null }
  if (!res.ok) throw new ApiError(payload?.error || 'request-failed', payload?.message || `Request failed (HTTP ${res.status}).`, { status: res.status, body: payload })
  if (payload === null) throw new ApiError('bad-response', 'The server sent something this app could not read.', { status: res.status })
  return payload
}

export const exchangeToken = (token, opts) => apiFetch('/auth/exchange', { ...opts, method: 'POST', body: { token } })
export const getMeta = (opts) => apiFetch('/meta', opts)
export const getAgents = (opts) => apiFetch('/agents', opts)
export const getTranscript = (threadId, opts = {}) => apiFetch(`/agents/${encodeURIComponent(threadId)}/transcript`, { ...opts, query: { limit: opts.limit, maxBytes: opts.maxBytes } })
export const sendPrompt = (threadId, { text = '', attachments = [] } = {}, opts) => apiFetch(`/agents/${encodeURIComponent(threadId)}/send`, {
  ...opts, method: 'POST', body: { text, attachments, mode: 'queue' },
})
export const forkAgent = (threadId, { title = '' } = {}, opts) => apiFetch(`/agents/${encodeURIComponent(threadId)}/fork`, {
	...opts, method: 'POST', body: { title },
})
export const startProjectAgent = (threadId, { prompt, title = '', backend = '', providerId = '', model = '', effort = '', isolation = '' }, opts) => apiFetch(`/agents/${encodeURIComponent(threadId)}/new`, {
  ...opts, method: 'POST', body: { prompt, title, backend, providerId, model, effort, isolation },
})
export const getProjectLaunchOptions = (threadId, { backend, providerId } = {}, opts = {}) => apiFetch(`/agents/${encodeURIComponent(threadId)}/launch-options`, { ...opts, query: { backend, providerId } })
export const listWorktreeFiles = (threadId, path = '', opts = {}) => apiFetch(`/agents/${encodeURIComponent(threadId)}/files`, { ...opts, query: { path } })
export const readWorktreeFile = (threadId, path, opts = {}) => apiFetch(`/agents/${encodeURIComponent(threadId)}/file`, { ...opts, query: { path } })
export const worktreeImageUrl = (threadId, path) => {
  const query = new URLSearchParams({ path: String(path || '') })
  return `${API_BASE}/agents/${encodeURIComponent(threadId)}/file/image?${query}`
}
export const writeWorktreeFile = (threadId, body, opts) => apiFetch(`/agents/${encodeURIComponent(threadId)}/file`, { ...opts, method: 'PUT', body })
export const interruptAgent = (threadId, opts) => apiFetch(`/agents/${encodeURIComponent(threadId)}/interrupt`, { ...opts, method: 'POST', body: {} })
export const stopAgent = (threadId, opts) => apiFetch(`/agents/${encodeURIComponent(threadId)}/stop`, { ...opts, method: 'POST', body: {} })
export const getDiff = (threadId, opts = {}) => apiFetch(`/agents/${encodeURIComponent(threadId)}/diff`, { ...opts, query: { maxBytes: opts.maxBytes, maxLines: opts.maxLines } })
export const getPermission = (requestId, opts) => apiFetch(`/permissions/${encodeURIComponent(requestId)}`, opts)
export const respondPermission = (requestId, answer, opts) => apiFetch(`/permissions/${encodeURIComponent(requestId)}`, { ...opts, method: 'POST', body: answer })
export const logout = (opts) => apiFetch('/auth/logout', { ...opts, method: 'POST', body: {} })
export function eventsUrl({ scope = 'roster', threadId, lastEventId } = {}) {
  const q = new URLSearchParams({ scope })
  if (scope === 'thread' && threadId) q.set('threadId', threadId)
  if (lastEventId) q.set('lastEventId', String(lastEventId))
  return `${API_BASE}/events?${q}`
}
