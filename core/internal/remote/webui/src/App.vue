<script setup>
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import MarkdownBlock from './components/MarkdownBlock.vue'
import { bootstrapAuth } from './api/auth.js'
import {
  API_VERSION, eventsUrl, getAgents, getMeta, getPermission, getTranscript,
  interruptAgent, logout, respondPermission, stopAgent,
} from './api/client.js'

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
const answers = ref({})
let stream = null

const selected = computed(() => agents.value.find((a) => a.threadId === selectedID.value) ?? null)
const prompt = computed(() => selected.value?.awaitingPermission ?? null)
const versionSkew = computed(() => Number(meta.value?.apiVersion) !== API_VERSION)

boot()
onBeforeUnmount(closeStream)
watch(() => prompt.value?.requestId, async (requestID) => {
  detail.value = null
  answers.value = {}
  if (!requestID) return
  try {
    const next = await getPermission(requestID)
    // The server has already reduced this to a plan or a typed question list;
    // normal tool prompts never have renderable input here.
    detail.value = next
    for (const q of next.questions ?? []) answers.value[q?.question] = q?.multiSelect ? [] : ''
  } catch (err) { actionError.value = err?.message || 'Could not load that permission request.' }
})

async function boot() {
  phase.value = 'booting'; message.value = ''
  const auth = await bootstrapAuth()
  if (auth.error) { phase.value = auth.error.isAuth ? 'bad-token' : 'offline'; message.value = auth.error.message; return }
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
  stream.addEventListener('roster', event => applyRoster(event))
  stream.addEventListener('turnState', event => applyTurnState(event))
  stream.addEventListener('permissionRequested', event => refresh().catch(() => {}))
  stream.addEventListener('permissionResolved', event => refresh().catch(() => {}))
  stream.addEventListener('agentGone', event => {
    const body = parse(event); agents.value = agents.value.filter(a => a.threadId !== body?.threadId)
    if (body?.threadId === selectedID.value) back()
  })
  stream.addEventListener('agentEvent', event => {
    const body = parse(event)
    if (body?.threadId === selectedID.value && Array.isArray(body.events)) transcript.value.push(...body.events)
  })
  stream.addEventListener('gap', () => { if (selectedID.value) open(selectedID.value); else refresh().catch(() => {}) })
  stream.addEventListener('revoked', event => { message.value = parse(event)?.reason || 'This device was unpaired.'; phase.value = 'revoked'; closeStream() })
}
function parse(event) { try { return JSON.parse(event.data) } catch { return null } }
function applyRoster(event) { const body = parse(event); if (Array.isArray(body?.agents)) agents.value = body.agents }
function applyTurnState(event) {
  const state = parse(event); const row = agents.value.find(a => a.threadId === state?.threadId)
  if (!row) return
  if (typeof state.busy === 'boolean') row.busy = state.busy
  if (typeof state.attention === 'boolean') row.attention = state.attention
  if ('awaitingPermission' in state) row.awaitingPermission = state.awaitingPermission
}

async function open(threadId) {
  selectedID.value = threadId; actionError.value = ''
  try {
    const body = await getTranscript(threadId, { limit: 500, maxBytes: 262144 })
    transcript.value = Array.isArray(body?.events) ? body.events : []
    transcriptTruncated.value = !!body?.truncated
    lastEventID.value = Number(body?.lastEventId) || 0
    connectThread(threadId)
  } catch (err) { actionError.value = err?.message || 'Could not load this conversation.' }
}
function back() { selectedID.value = ''; transcript.value = []; detail.value = null; connectRoster() }
async function control(op) {
  if (!selected.value) return
  actionError.value = ''
  try { await (op === 'interrupt' ? interruptAgent(selected.value.threadId) : stopAgent(selected.value.threadId)); await refresh() }
  catch (err) { actionError.value = err?.message || `Could not ${op} this agent.` }
}
function pick(question, option, multi) {
  const current = answers.value[question] ?? (multi ? [] : '')
  if (!multi) answers.value = { ...answers.value, [question]: option }; return
  const next = Array.isArray(current) ? [...current] : []
  const at = next.indexOf(option); if (at >= 0) next.splice(at, 1); else next.push(option)
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
</script>

<template>
  <main v-if="phase !== 'ready'" class="shell empty">
    <h1>Agent Kate Remote Access</h1>
    <p v-if="phase === 'booting'">Connecting securely…</p>
    <template v-else>
      <p>{{ message || (phase === 'unauthenticated' ? 'This device is not paired.' : 'Remote access is unavailable.') }}</p>
      <p v-if="phase === 'unauthenticated' || phase === 'bad-token'">Open a new pairing link from the desktop Remote Access panel.</p>
      <button v-if="phase === 'offline'" @click="boot">Try again</button>
    </template>
  </main>

  <main v-else class="shell">
    <header class="bar"><button v-if="selected" class="ghost" @click="back">← Agents</button><h1>Agent Kate</h1><span class="spacer" /><button class="ghost" @click="refresh">Refresh</button><button class="ghost" @click="doLogout">Log out</button></header>
    <p v-if="versionSkew" class="warning">This browser bundle is for API v{{ API_VERSION }} while the core serves v{{ meta?.apiVersion }}. Reload after updating the desktop.</p>
    <p v-if="actionError" class="error">{{ actionError }}</p>

    <section v-if="!selected">
      <p class="notice">This paired device may view typed, redacted events and answer allowed human prompts. It is not a desktop UI or agent bridge.</p>
      <div v-if="!agents.length" class="empty">No agents are available. Start or resume one from the desktop.</div>
      <div v-else class="grid"><button v-for="agent in agents" :key="agent.threadId" class="card" :class="{ attention: agent.attention }" @click="open(agent.threadId)"><strong>{{ agent.title || agent.threadId }}</strong><div class="meta">{{ agent.project }} · {{ agent.engineName || agent.backend }} · {{ agent.model }}</div><div class="meta">{{ agent.busy ? 'Working' : 'Idle' }}<span v-if="agent.awaitingPermission"> · needs your response</span></div></button></div>
    </section>

    <section v-else>
      <div class="bar"><div><strong>{{ selected.title || selected.threadId }}</strong><div class="meta">{{ selected.busy ? 'Working' : 'Idle' }} · {{ selected.model }}</div></div><span class="spacer" /><button v-if="selected.busy" @click="control('interrupt')">Interrupt</button><button class="danger" @click="control('stop')">Stop</button></div>
      <p class="notice">Remote message sending is not available until accepted messages have one canonical desktop and remote transcript echo.</p>
      <p v-if="transcriptTruncated" class="warning">Some conversation content was clipped for this phone.</p>
      <div class="conversation"><div v-if="!transcript.length" class="empty">No remote-safe transcript events yet.</div><article v-for="(event, index) in transcript" :key="`${index}-${event.at || ''}`" class="event" :class="event.kind"><MarkdownBlock v-if="event.kind === 'user' || event.kind === 'assistant'" :source="event.text" /><template v-else-if="event.kind === 'tool'"><strong>{{ event.toolName || 'Tool' }}</strong><div>{{ event.summary || 'Tool activity' }}</div></template><template v-else><strong>Agent status</strong><div>{{ event.text }}</div></template></article></div>
      <aside v-if="prompt" class="permission"><div class="topline"><strong>{{ prompt.kind === 'plan' ? 'Plan approval' : prompt.kind === 'question' ? 'Question for you' : 'Permission needed' }}</strong><span class="spacer" /><span class="meta">{{ prompt.toolName }}</span></div><p>{{ prompt.summary }}</p><MarkdownBlock v-if="detail?.kind === 'plan' && detail.plan" :source="detail.plan" /><template v-else-if="detail?.kind === 'question'"><fieldset v-for="question in detail.questions || []" :key="question.question"><strong>{{ question.question }}</strong><label v-for="option in question.options || []" :key="optionLabel(option)"><input :type="question.multiSelect ? 'checkbox' : 'radio'" :name="question.question" :checked="chosen(question.question, optionLabel(option))" @change="pick(question.question, optionLabel(option), question.multiSelect)" /> {{ optionLabel(option) }}<span v-if="optionDescription(option)" class="meta"> — {{ optionDescription(option) }}</span></label></fieldset></template><p v-else-if="prompt.kind !== 'tool'" class="warning">The allowed details are unavailable. Answer this on the desktop instead.</p><div class="actions"><button class="good" :disabled="prompt.kind === 'question' && !detail" @click="answer(true)">Approve<span v-if="prompt.kind === 'question'"> answers</span></button><button class="danger" @click="answer(false)">Deny</button></div></aside>
    </section>
  </main>
</template>
