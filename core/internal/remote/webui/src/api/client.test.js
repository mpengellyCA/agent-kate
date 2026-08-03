import { describe, expect, it } from 'vitest'
import { sendPrompt } from './client.js'

describe('sendPrompt', () => {
  it('always uses the queued, typed remote-send contract', async () => {
    let call
    const response = await sendPrompt('thread / one', {
      text: 'continue safely',
      attachments: [{ kind: 'text', name: 'notes.md', mediaType: 'text/markdown', text: '# note' }],
    }, {
      fetchImpl: async (url, init) => {
        call = { url, init }
        return new Response(JSON.stringify({ ok: true, queued: true, position: 1 }), { status: 200 })
      },
    })

    expect(response.queued).toBe(true)
    expect(call.url).toBe('/api/v1/agents/thread%20%2F%20one/send')
    expect(call.init.method).toBe('POST')
    expect(JSON.parse(call.init.body)).toEqual({
      text: 'continue safely',
      attachments: [{ kind: 'text', name: 'notes.md', mediaType: 'text/markdown', text: '# note' }],
      mode: 'queue',
    })
  })
})
