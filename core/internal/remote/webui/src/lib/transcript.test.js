import { describe, expect, it } from 'vitest'
import { displayTranscript } from './transcript.js'

describe('displayTranscript', () => {
  it('keeps formatted conversation content and suppresses status mirrors', () => {
    const assistant = { kind: 'assistant', text: '## Completed\n\nFormatted output.' }
    expect(displayTranscript([
      { kind: 'user', text: 'Please complete this.' },
      assistant,
      { kind: 'lifecycle', text: assistant.text },
      { kind: 'unknown', text: assistant.text },
      { kind: 'tool', toolName: 'Bash', summary: 'Approve a shell command' },
    ])).toEqual([
      { kind: 'user', text: 'Please complete this.' },
      assistant,
      { kind: 'tool', toolName: 'Bash', summary: 'Approve a shell command' },
    ])
  })
})
