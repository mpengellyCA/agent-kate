<script setup>
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import MarkdownBlock from './components/MarkdownBlock.vue'
import { bootstrapAuth } from './api/auth.js'
import {
  API_VERSION, eventsUrl, getAgents, getMeta, getPermission, getTranscript,
  interruptAgent, logout, respondPermission, sendPrompt, stopAgent,
} from './api/client.js'

const MAX_ATTACHMENTS = 4
const MAX_ATTACHMENT_BYTES = 4 * 1024 * 1024
const MAX_ATTACHMENT_TOTAL = 6 * 1024 * 1024
const imageTypes = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp'])

const phase = ref('booting')
const message = ref('')
const meta = ref(null)
const agents = ref([])
const selectedID = ref('')
const transcript = ref([])
const transcriptTruncated = ref(false)
const lastEventID = ref(0)
const detail = ref(null)
const actionError = ref('')
const sendFeedback = ref('')
const answers = ref({})
const drawerOpen = ref(false)
const selectedProject = ref('')
const projectFilter = ref('')
const composer = ref('')
const attachments = ref([])
const attachmentPicker = ref(null)
const sending = ref(false)
const conversation = ref(null)
let stream = null

const selected = computed(() => agents.value.find((agent) => agent.threadId === selectedID.value) ?? null)
const prompt = computed(() => selected.value?.awaitingPermission ?? null)
const versionSkew = computed(() => Number(meta.value?.apiVersion) !== API_VERSION)
const activeAgents = computed(() => agents.value.filter((agent) => agent.status !== 'archived'))
const projectGroups = computed(() => {
  const groups = new Map()
  for (const agent of agents.value) {
    const name = projectName(agent)
    if (!groups.has(name)) groups.set(name, { name, agents: [] })
    groups.get(name).agents.push(agent)
  }
  return [...groups.values()]
    .map((group) => {
      group.agents.sort((a, b) => activityTime(b) - activityTime(a))
      group.lastActivity = activityTime(group.agents[0])
      group.activeCount = group.agents.filter((agent) => agent.status !== 'archived').length
      group.busyCount = group.agents.filter((agent) => agent.busy).length
      group.attentionCount = group.agents.filter((agent) => agent.attention).length
      return group
    })
    .sort((a, b) => b.lastActivity - a.lastActivity || a.name.localeCompare(b.name))
})
const visibleProjectGroups = computed(() => {
  const query = projectFilter.value.trim().toLocaleLowerCase()
  if (!query) return projectGroups.value
  return projectGroups.value.filter((group) => group.name.toLocaleLowerCase().includes(query) ||
    group.agents.some((agent) => String(agent.title || '').toLocaleLowerCase().includes(query)))
})
const currentProject = computed(() => projectGroups.value.find((group) => group.name === selectedProject.value) ?? null)
const canSend = computed(() => !!selected.value && !sending.value &&
  (!!composer.value.trim() || attachments.value.length > 0) && selected.value.status !== 'archived')
const attachmentBytes = computed(() => attachments.value.reduce((total, attachment) => total + attachment.bytes, 0))

boot()
onBeforeUnmount(closeStream)
watch(() => prompt.value?.requestId, async (requestID) => {
  detail.value = null
  answers.value = {}
  if (!requestID) return
  try {
    const next = await getPermission(requestID)
    // The server has already reduced this to a plan or typed questions. A raw
    // tool input has no field anywhere in this rendering path.
    detail.value = next
    for (const question of next.questions ?? []) answers.value[question?.question] = question?.multiSelect ? [] : ''
  } catch (err) { actionError.value = err?.message || 'Could not load that permission request.' }
})
watch(() => transcript.value.length, () => scrollConversation())

async function boot() {
  phase.value = 'booting'; message.value = ''
  const auth = await bootstrapAuth()
  if (auth.error) {
    phase.value = auth.error.isAuth ? 'bad-token' : 'offline'
    message.value = auth.error.message
    return
  }
  try {
    meta.value = await getMeta()
    await refresh()
    phase.value = 'ready'
    connectRoster()
  } catch (err) {
    phase.value = err?.isAuth ? 'unauthenticated' : 'offline'
    message.value = err?.message || 'Cannot reach Agent Kate.'
  }
}

async function refresh() {
  const body = await getAgents()
  agents.value = Array.isArray(body?.agents) ? body.agents : []
}

function closeStream() { if (stream) stream.close(); stream = null }
function connectRoster() { connect({ scope: 'roster' }) }
function connectThread(threadId) { connect({ scope: 'thread', threadId, lastEventId: lastEventID.value }) }
function connect(scope) {
  closeStream()
  stream = new EventSource(eventsUrl(scope))
  stream.addEventListener('roster', (event) => applyRoster(event))
  stream.addEventListener('turnState', (event) => applyTurnState(event))
  stream.addEventListener('permissionRequested', () => refresh().catch(() => {}))
  stream.addEventListener('permissionResolved', () => refresh().catch(() => {}))
  stream.addEventListener('agentGone', (event) => {
    const body = parse(event)
    agents.value = agents.value.filter((agent) => agent.threadId !== body?.threadId)
    if (body?.threadId === selectedID.value) back()
  })
  stream.addEventListener('agentEvent', (event) => {
    const body = parse(event)
    rememberEventID(event)
    if (body?.threadId === selectedID.value && Array.isArray(body.events)) transcript.value.push(...body.events)
  })
  stream.addEventListener('gap', () => { if (selectedID.value) open(selectedID.value); else refresh().catch(() => {}) })
  stream.addEventListener('revoked', (event) => {
    message.value = parse(event)?.reason || 'This device has been unpaired.'
    phase.value = 'revoked'
    closeStream()
  })
}
function parse(event) { try { return JSON.parse(event.data) } catch { return null } }
function rememberEventID(event) {
  const id = Number(event?.lastEventId)
  if (Number.isFinite(id) && id > lastEventID.value) lastEventID.value = id
}
function applyRoster(event) { const body = parse(event); if (Array.isArray(body?.agents)) agents.value = body.agents }
function applyTurnState(event) {
  const state = parse(event)
  const row = agents.value.find((agent) => agent.threadId === state?.threadId)
  if (!row) return
  if (typeof state.busy === 'boolean') row.busy = state.busy
  if (typeof state.attention === 'boolean') row.attention = state.attention
  if ('awaitingPermission' in state) row.awaitingPermission = state.awaitingPermission
}

async function open(threadId) {
  const agent = agents.value.find((row) => row.threadId === threadId)
  if (agent) selectedProject.value = projectName(agent)
  selectedID.value = threadId
  actionError.value = ''
  sendFeedback.value = ''
  drawerOpen.value = false
  try {
    const body = await getTranscript(threadId, { limit: 500, maxBytes: 262144 })
    transcript.value = Array.isArray(body?.events) ? body.events : []
    transcriptTruncated.value = !!body?.truncated
    lastEventID.value = Number(body?.lastEventId) || 0
    connectThread(threadId)
    scrollConversation()
  } catch (err) { actionError.value = err?.message || 'Could not load this conversation.' }
}
function back() {
  selectedID.value = ''
  transcript.value = []
  detail.value = null
  composer.value = ''
  attachments.value = []
  connectRoster()
}
function openProject(name) {
  selectedID.value = ''
  transcript.value = []
  detail.value = null
  selectedProject.value = name
  drawerOpen.value = false
  connectRoster()
}
function showProjects() {
  selectedID.value = ''
  transcript.value = []
  detail.value = null
  selectedProject.value = ''
  projectFilter.value = ''
  connectRoster()
}
function scrollConversation() {
  nextTick(() => {
    const pane = conversation.value
    if (pane) pane.scrollTop = pane.scrollHeight
  })
}

async function control(op) {
  if (!selected.value) return
  actionError.value = ''
  try {
    await (op === 'interrupt' ? interruptAgent(selected.value.threadId) : stopAgent(selected.value.threadId))
    await refresh()
  } catch (err) { actionError.value = err?.message || `Could not ${op} this agent.` }
}

function openAttachmentPicker() { attachmentPicker.value?.click() }
async function addAttachments(event) {
  const files = Array.from(event.target?.files ?? [])
  if (attachmentPicker.value) attachmentPicker.value.value = ''
  actionError.value = ''
  for (const file of files) {
    if (attachments.value.length >= MAX_ATTACHMENTS) {
      actionError.value = `A remote message can have at most ${MAX_ATTACHMENTS} attachments.`
      break
    }
    try {
      const attachment = await encodeAttachment(file)
      if (attachmentBytes.value + attachment.bytes > MAX_ATTACHMENT_TOTAL) {
        actionError.value = 'Attachments together must be 6 MiB or smaller.'
        break
      }
      attachments.value.push(attachment)
    } catch (err) { actionError.value = err?.message || `Could not add ${file.name || 'that file'}.` }
  }
}
async function encodeAttachment(file) {
  const name = String(file?.name || '').trim()
  if (!name || name.length > 160 || /[\\/\0]/.test(name) || name === '.' || name === '..') {
    throw new Error('That attachment name is not safe to send.')
  }
  if (!Number.isFinite(file?.size) || file.size > MAX_ATTACHMENT_BYTES) {
    throw new Error(`${name} is larger than the 4 MiB per-file limit.`)
  }
  if (imageTypes.has(file.type)) {
    const dataURL = await readDataURL(file)
    const dataB64 = dataURL.slice(dataURL.indexOf(',') + 1)
    return { kind: 'image', name, mediaType: file.type, dataB64, bytes: file.size }
  }
  if (file.type.startsWith('text/') || /\.(md|markdown|txt|log|json|yaml|yml|toml|csv)$/i.test(name)) {
    const text = await file.text()
    const bytes = new TextEncoder().encode(text).byteLength
    if (bytes > MAX_ATTACHMENT_BYTES) throw new Error(`${name} is larger than the 4 MiB per-file limit.`)
    const mediaType = /\.(md|markdown)$/i.test(name) ? 'text/markdown' : 'text/plain'
    return { kind: 'text', name, mediaType, text, bytes }
  }
  throw new Error('Attach a PNG, JPEG, GIF, WebP, plain-text, or Markdown file.')
}
function readDataURL(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result || ''))
    reader.onerror = () => reject(new Error(`Could not read ${file.name}.`))
    reader.readAsDataURL(file)
  })
}
function removeAttachment(index) { attachments.value.splice(index, 1) }
function attachmentRequest() {
  // bytes is a browser-only UI measurement. The wire DTO is explicitly typed
  // and cannot carry a path, local URL, cache key, or arbitrary file metadata.
  return attachments.value.map(({ kind, name, mediaType, text, dataB64 }) => ({ kind, name, mediaType, text, dataB64 }))
}
async function send() {
  if (!canSend.value || !selected.value) return
  sending.value = true
  actionError.value = ''
  sendFeedback.value = ''
  try {
    const result = await sendPrompt(selected.value.threadId, { text: composer.value.trim(), attachments: attachmentRequest() })
    composer.value = ''
    attachments.value = []
    sendFeedback.value = result?.resuming
      ? 'Waking the agent — your message will be delivered once its saved session is ready.'
      : result?.queued && result?.position > 1
        ? `Queued — message ${result.position} will run after the current turn.`
        : selected.value.busy ? 'Queued for the next turn.' : 'Accepted by the agent.'
  } catch (err) {
    // Preserve the human's draft and browser-held attachment data on a failed
    // request. A transport failure must never silently eat their message.
    actionError.value = err?.message || 'Could not send that message.'
  } finally { sending.value = false }
}

function pick(question, option, multi) {
  const current = answers.value[question] ?? (multi ? [] : '')
  if (!multi) { answers.value = { ...answers.value, [question]: option }; return }
  const next = Array.isArray(current) ? [...current] : []
  const at = next.indexOf(option)
  if (at >= 0) next.splice(at, 1); else next.push(option)
  answers.value = { ...answers.value, [question]: next }
}
function chosen(question, option) { const value = answers.value[question]; return Array.isArray(value) ? value.includes(option) : value === option }
function optionLabel(option) { return typeof option === 'string' ? option : String(option?.label ?? option?.name ?? '') }
function optionDescription(option) { return typeof option === 'object' ? String(option?.description ?? '') : '' }
async function answer(allow) {
  if (!prompt.value) return
  actionError.value = ''
  const body = { allow }
  if (allow && detail.value?.kind === 'question') body.updatedInput = { questions: detail.value.questions, answers: answers.value }
  try { await respondPermission(prompt.value.requestId, body); await refresh(); detail.value = null }
  catch (err) { actionError.value = err?.message || 'Could not send that answer.' }
}
async function doLogout() { try { await logout() } catch {} phase.value = 'unauthenticated'; closeStream() }

function agentState(agent) {
  if (agent.awaitingPermission) return 'Needs you'
  if (agent.busy) return 'Working'
  if (agent.status === 'dormant') return 'Dormant'
  return 'Ready'
}
function shortBytes(bytes) { return bytes >= 1024 * 1024 ? `${(bytes / 1024 / 1024).toFixed(1)} MiB` : `${Math.ceil(bytes / 1024)} KiB` }
function projectName(agent) { return String(agent?.project || '').trim() || 'Unassigned project' }
function activityTime(agent) { const value = Date.parse(agent?.lastActivityAt || ''); return Number.isFinite(value) ? value : 0 }
function projectMark(name) { return name.trim().slice(0, 1).toLocaleUpperCase() || '•' }
function relativeActivity(value) {
  if (!value) return 'No recent activity'
  const minutes = Math.max(0, Math.round((Date.now() - value) / 60000))
  if (minutes < 1) return 'Active now'
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.round(hours / 24)
  return `${days}d ago`
}
</script>

<template>
  <main v-if="phase !== 'ready'" class="grid min-h-dvh place-items-center bg-base-100 px-5 pt-safe pb-safe">
    <section class="card w-full max-w-sm border border-ak-border bg-base-200 shadow-xl">
      <div class="card-body items-center text-center">
        <span class="grid h-12 w-12 place-items-center rounded-2xl bg-primary text-2xl text-primary-content">K</span>
        <template v-if="phase === 'booting'"><span class="loading loading-spinner loading-md text-primary" /><p class="text-sm text-ak-muted">Connecting securely…</p></template>
        <template v-else>
          <h1 class="card-title">Agent Kate Remote Access</h1>
          <p class="text-sm text-ak-muted">{{ message || (phase === 'unauthenticated' ? 'This device is not paired.' : 'Remote access is unavailable.') }}</p>
          <p v-if="phase === 'unauthenticated' || phase === 'bad-token'" class="text-xs text-ak-muted">Open a new one-time pairing link from the desktop Remote Access panel.</p>
          <button v-if="phase === 'offline'" type="button" class="btn btn-primary btn-sm mt-2" @click="boot">Try again</button>
        </template>
      </div>
    </section>
  </main>

  <main v-else class="drawer lg:drawer-open">
    <input id="agents-drawer" v-model="drawerOpen" type="checkbox" class="drawer-toggle" />
    <div class="drawer-content flex h-dvh min-w-0 flex-col overflow-hidden bg-base-100">
      <header class="flex flex-none items-center gap-2 border-b border-ak-border bg-base-200 px-3 py-2 pt-safe">
        <label for="agents-drawer" class="btn btn-ghost btn-sm btn-square lg:hidden" aria-label="Show projects">☰</label>
        <button v-if="selected" type="button" class="btn btn-ghost btn-sm lg:hidden" @click="back">Project</button>
        <button v-else-if="selectedProject" type="button" class="btn btn-ghost btn-sm lg:hidden" @click="showProjects">Projects</button>
        <div class="min-w-0"><h1 class="truncate text-sm font-semibold">{{ selected ? (selected.title || 'Agent') : selectedProject || 'Agent Kate' }}</h1><p class="truncate text-xs text-ak-muted">{{ selected ? selectedProject : selectedProject ? 'Project agents' : 'Paired remote device' }}</p></div>
        <span class="flex-1" />
        <span v-if="agents.filter(a => a.attention).length" class="badge badge-warning badge-sm hidden sm:inline-flex">{{ agents.filter(a => a.attention).length }} need you</span>
        <button type="button" class="btn btn-ghost btn-sm btn-square min-[460px]:w-auto" aria-label="Refresh projects" @click="refresh"><span aria-hidden="true">↻</span><span class="hidden min-[460px]:inline">Refresh</span></button>
        <button type="button" class="btn btn-ghost btn-sm btn-square min-[460px]:w-auto" aria-label="Log out from this paired device" @click="doLogout"><span aria-hidden="true">⇥</span><span class="hidden min-[460px]:inline">Log out</span></button>
      </header>

      <div v-if="versionSkew" class="alert alert-info rounded-none px-3 py-2 text-xs"><span>This app was built for API v{{ API_VERSION }} while the core serves v{{ meta?.apiVersion }}. Reload after updating the desktop.</span></div>
      <div v-if="actionError" class="alert alert-error mx-3 mt-3 flex-none py-2 text-sm"><span>{{ actionError }}</span></div>

      <section v-if="!selected" class="min-h-0 flex-1 overflow-y-auto px-safe">
        <div class="mx-auto max-w-5xl p-4 sm:p-6">
          <div v-if="!selectedProject" class="alert alert-info mb-5 text-sm"><span>This paired device sees only typed, redacted agent events. It is not a desktop UI or an agent bridge.</span></div>

          <template v-if="!agents.length">
            <div class="hero min-h-64 rounded-box border border-dashed border-ak-border bg-base-200"><div class="hero-content text-center"><div><h2 class="text-lg font-semibold">No projects yet</h2><p class="mt-2 text-sm text-ak-muted">Start or resume an agent from the desktop; its project will appear here.</p></div></div></div>
          </template>

          <template v-else-if="!selectedProject">
            <div class="mb-5 flex flex-wrap items-end justify-between gap-3"><div><p class="text-xs font-semibold uppercase tracking-[0.16em] text-primary">Remote workspace</p><h2 class="mt-1 text-2xl font-semibold tracking-tight">Recent projects</h2><p class="mt-1 text-sm text-ak-muted">Choose a project, then jump into any agent.</p></div><span class="badge badge-outline">{{ projectGroups.length }} projects</span></div>
            <label class="input input-bordered mb-4 flex h-11 items-center gap-2 bg-base-200"><span class="text-ak-muted" aria-hidden="true">⌕</span><input v-model="projectFilter" class="min-w-0 grow" type="search" placeholder="Find a project or agent" aria-label="Find a project or agent" /></label>
            <div v-if="visibleProjectGroups.length" class="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              <button v-for="group in visibleProjectGroups" :key="group.name" type="button" class="project-card card border border-ak-border bg-base-200 text-left shadow-sm transition" :class="{ 'border-warning/70': group.attentionCount }" @click="openProject(group.name)">
                <span class="card-body gap-3 p-4"><span class="flex items-start gap-3"><span class="grid h-10 w-10 flex-none place-items-center rounded-xl bg-primary/20 text-base font-semibold text-primary">{{ projectMark(group.name) }}</span><span class="min-w-0 flex-1"><span class="block truncate font-semibold">{{ group.name }}</span><span class="mt-1 block text-xs text-ak-muted">{{ group.activeCount }} active {{ group.activeCount === 1 ? 'agent' : 'agents' }} · {{ relativeActivity(group.lastActivity) }}</span></span><span v-if="group.attentionCount" class="badge badge-warning badge-sm">{{ group.attentionCount }}</span></span><span class="project-preview"><span v-for="agent in group.agents.slice(0, 2)" :key="agent.threadId" class="block truncate text-xs"><span class="mr-1 text-primary">{{ agent.busy ? '●' : '○' }}</span>{{ agent.title || agent.threadId }}</span><span v-if="group.agents.length > 2" class="mt-1 block text-xs text-ak-muted">+{{ group.agents.length - 2 }} more agent{{ group.agents.length === 3 ? '' : 's' }}</span></span></span>
              </button>
            </div>
            <div v-else class="rounded-box border border-dashed border-ak-border p-8 text-center text-sm text-ak-muted">No matching projects or agents.</div>
          </template>

          <template v-else>
            <div class="mb-5 flex items-start gap-3"><button type="button" class="btn btn-ghost btn-square btn-sm mt-0.5" aria-label="All projects" @click="showProjects">←</button><span class="grid h-11 w-11 flex-none place-items-center rounded-xl bg-primary/20 text-lg font-semibold text-primary">{{ projectMark(selectedProject) }}</span><div class="min-w-0"><p class="text-xs font-semibold uppercase tracking-[0.16em] text-primary">Project</p><h2 class="truncate text-2xl font-semibold tracking-tight">{{ selectedProject }}</h2><p class="mt-1 text-sm text-ak-muted">{{ currentProject?.agents.length || 0 }} recent agents · {{ relativeActivity(currentProject?.lastActivity || 0) }}</p></div></div>
            <div v-if="currentProject" class="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
              <button v-for="agent in currentProject.agents" :key="agent.threadId" type="button" class="agent-row card border border-ak-border bg-base-200 text-left shadow-sm transition" :class="{ 'border-warning/70': agent.attention }" @click="open(agent.threadId)">
                <span class="card-body gap-2 p-3"><span class="flex min-w-0 items-start gap-2"><span class="mt-1.5 h-2 w-2 flex-none rounded-full" :class="agent.attention ? 'bg-warning' : agent.busy ? 'bg-primary' : agent.status === 'archived' ? 'bg-base-content/30' : 'bg-success'" /><span class="min-w-0 flex-1"><span class="agent-title block font-medium">{{ agent.title || agent.threadId }}</span><span class="mt-1 block truncate text-xs text-ak-muted">{{ agent.engineName || agent.backend || 'Agent' }}<template v-if="agent.model"> · {{ agent.model }}</template></span></span><span class="badge badge-sm" :class="agent.attention ? 'badge-warning' : agent.busy ? 'badge-primary' : 'badge-ghost'">{{ agentState(agent) }}</span></span><span v-if="agent.awaitingPermission" class="truncate rounded-md bg-warning/10 px-2 py-1 text-xs text-warning">{{ agent.awaitingPermission.summary || agent.awaitingPermission.toolName }}</span><span class="text-xs text-ak-muted">{{ relativeActivity(activityTime(agent)) }}</span></span>
              </button>
            </div>
            <div v-else class="rounded-box border border-dashed border-ak-border p-8 text-center text-sm text-ak-muted">This project is no longer in the remote roster.</div>
          </template>
        </div>
      </section>

      <section v-else class="flex min-h-0 flex-1 flex-col overflow-hidden">
        <header class="flex flex-none items-center gap-2 border-b border-ak-border bg-base-200 px-3 py-2"><button type="button" class="btn btn-ghost btn-sm hidden lg:inline-flex" @click="back">← {{ selectedProject }}</button><div class="min-w-0"><h2 class="truncate text-sm font-semibold">{{ selected.title || selected.threadId }}</h2><p class="truncate text-xs text-ak-muted">{{ selected.engineName || selected.backend }} · {{ selected.busy ? 'Working' : selected.status === 'dormant' ? 'Dormant' : 'Ready' }}</p></div><span class="flex-1" /><button v-if="selected.busy" type="button" class="btn btn-warning btn-sm" @click="control('interrupt')">Interrupt</button><button type="button" class="btn btn-outline btn-error btn-sm" @click="control('stop')">Stop</button></header>
        <p v-if="transcriptTruncated" class="mx-3 mt-3 flex-none rounded-box bg-warning/10 px-3 py-2 text-xs text-warning">Some conversation content was clipped for this phone.</p>
        <div ref="conversation" class="min-h-0 flex-1 overflow-y-auto overscroll-contain px-safe">
          <div class="mx-auto flex max-w-4xl flex-col gap-3 p-3 sm:p-5">
            <div v-if="!transcript.length" class="py-16 text-center text-sm text-ak-muted">No remote-safe conversation events yet.</div>
            <article v-for="(event, index) in transcript" :key="`${index}-${event.at || ''}`" class="rounded-box border border-ak-border px-3 py-2 text-sm shadow-sm" :class="event.kind === 'user' ? 'ml-7 bg-primary/15 sm:ml-20' : event.kind === 'assistant' ? 'mr-3 bg-base-200 sm:mr-16' : 'bg-base-300 text-xs'">
              <MarkdownBlock v-if="event.kind === 'user' || event.kind === 'assistant'" :source="event.text" />
              <template v-else-if="event.kind === 'tool'"><p class="font-semibold">{{ event.toolName || 'Tool activity' }}</p><p class="mt-1 text-ak-muted">{{ event.summary || 'The agent is using a tool.' }}</p></template>
              <template v-else><p class="font-semibold">Agent status</p><p class="mt-1 text-ak-muted">{{ event.text }}</p></template>
            </article>
          </div>
        </div>
        <footer class="flex-none border-t border-ak-border bg-base-200 px-safe pt-2 pb-safe">
          <div class="mx-auto max-w-4xl px-3 pb-2"><p v-if="sendFeedback" class="mb-2 text-xs text-success">{{ sendFeedback }}</p><div v-if="attachments.length" class="mb-2 flex flex-wrap gap-1"><span v-for="(attachment, index) in attachments" :key="`${attachment.name}-${index}`" class="badge badge-outline gap-1 py-3"><span class="max-w-32 truncate">{{ attachment.name }}</span><span class="text-ak-muted">{{ shortBytes(attachment.bytes) }}</span><button type="button" class="ml-1 text-error" :aria-label="`Remove ${attachment.name}`" @click="removeAttachment(index)">×</button></span></div><input ref="attachmentPicker" type="file" multiple class="hidden" accept="image/png,image/jpeg,image/gif,image/webp,text/plain,text/markdown,.md,.markdown,.txt,.log,.json,.yaml,.yml,.toml,.csv" @change="addAttachments" /><div class="flex items-end gap-2"><button type="button" class="btn btn-ghost btn-square" :disabled="sending || attachments.length >= MAX_ATTACHMENTS" aria-label="Attach file" @click="openAttachmentPicker">＋</button><textarea v-model="composer" rows="1" class="textarea textarea-bordered min-h-12 flex-1 resize-none bg-base-100" :disabled="sending || selected.status === 'archived'" :placeholder="selected.busy ? 'Queue a follow-up…' : selected.status === 'dormant' ? 'Wake the agent with a message…' : 'Send a message…'" @keydown.ctrl.enter.prevent="send" @keydown.meta.enter.prevent="send" /><button type="button" class="btn btn-primary" :disabled="!canSend" @click="send"><span v-if="sending" class="loading loading-spinner loading-xs" />Send</button></div><p class="mt-1 text-xs text-ak-muted">Messages queue in order. Attach up to {{ MAX_ATTACHMENTS }} images or text files ({{ shortBytes(MAX_ATTACHMENT_TOTAL) }} total). Ctrl/⌘ Enter sends.</p></div>
        </footer>
      </section>

      <dialog v-if="prompt" class="modal modal-open"><div class="modal-box max-h-[85dvh] max-w-2xl overflow-y-auto"><h3 class="text-lg font-semibold">{{ prompt.kind === 'plan' ? 'Plan approval' : prompt.kind === 'question' ? 'Question for you' : 'Permission needed' }}</h3><p class="mt-2 text-sm text-ak-muted">{{ prompt.summary }}</p><MarkdownBlock v-if="detail?.kind === 'plan' && detail.plan" class="mt-4 rounded-box bg-base-200 p-3" :source="detail.plan" /><div v-else-if="detail?.kind === 'question'" class="mt-4 space-y-4"><fieldset v-for="question in detail.questions || []" :key="question.question" class="rounded-box border border-ak-border p-3"><legend class="px-1 font-medium">{{ question.question }}</legend><label v-for="option in question.options || []" :key="optionLabel(option)" class="mt-2 flex cursor-pointer items-start gap-2 text-sm"><input :type="question.multiSelect ? 'checkbox' : 'radio'" :name="question.question" class="checkbox checkbox-sm mt-0.5" :checked="chosen(question.question, optionLabel(option))" @change="pick(question.question, optionLabel(option), question.multiSelect)" /><span>{{ optionLabel(option) }}<small v-if="optionDescription(option)" class="block text-ak-muted">{{ optionDescription(option) }}</small></span></label></fieldset></div><p v-else-if="prompt.kind !== 'tool'" class="mt-4 rounded-box bg-warning/10 p-3 text-sm text-warning">The allowed details are unavailable. Answer this on the desktop instead.</p><div class="modal-action"><button type="button" class="btn btn-outline btn-error" @click="answer(false)">Deny</button><button type="button" class="btn btn-success" :disabled="prompt.kind === 'question' && !detail" @click="answer(true)">Approve<span v-if="prompt.kind === 'question'"> answers</span></button></div></div></dialog>
    </div>

    <aside class="drawer-side z-30"><label for="agents-drawer" aria-label="Close projects" class="drawer-overlay" /><nav class="flex min-h-full w-72 flex-col border-r border-ak-border bg-base-200 pt-safe"><div class="border-b border-ak-border p-4"><p class="text-xs font-semibold uppercase tracking-wider text-ak-muted">Projects</p><p class="mt-1 text-xs text-ak-muted">{{ projectGroups.length }} recent · {{ activeAgents.length }} active agents</p></div><div class="min-h-0 flex-1 overflow-y-auto p-2"><button type="button" class="mb-2 flex w-full items-center gap-2 rounded-box px-3 py-2 text-left text-sm hover:bg-base-300" :class="{ 'bg-primary/20': !selectedProject && !selected }" @click="showProjects"><span class="grid h-7 w-7 place-items-center rounded-lg bg-base-300 text-primary">⌂</span><span>All projects</span></button><p v-if="projectGroups.length" class="px-3 pb-1 pt-2 text-[0.65rem] font-semibold uppercase tracking-[0.16em] text-ak-muted">Recent</p><button v-for="group in projectGroups" :key="group.name" type="button" class="mb-1 flex w-full items-center gap-2 rounded-box px-3 py-2 text-left hover:bg-base-300" :class="{ 'bg-primary/20': group.name === selectedProject, 'ring-1 ring-warning': group.attentionCount }" @click="openProject(group.name)"><span class="grid h-7 w-7 flex-none place-items-center rounded-lg bg-primary/15 text-xs font-semibold text-primary">{{ projectMark(group.name) }}</span><span class="min-w-0 flex-1"><span class="block truncate text-sm">{{ group.name }}</span><span class="block truncate text-xs text-ak-muted">{{ group.activeCount }} active · {{ relativeActivity(group.lastActivity) }}</span></span><span v-if="group.attentionCount" class="badge badge-warning badge-xs">{{ group.attentionCount }}</span></button><p v-if="!projectGroups.length" class="p-4 text-sm text-ak-muted">No recent projects.</p></div></nav></aside>
  </main>
</template>
