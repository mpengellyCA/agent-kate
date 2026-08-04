import { afterEach, describe, expect, it } from 'vitest'
import { router } from './router.js'

afterEach(async () => {
  await router.replace({ name: 'projects' })
})

describe('remote history routes', () => {
  it('keeps project locations in the path rather than a query string', () => {
    const resolved = router.resolve({ name: 'project', params: { project: 'Agent Kate & Co' } })
    expect(resolved.href).toBe('/projects/Agent%20Kate%20&%20Co')
  })

  it('adds project and agent selections to browser history', async () => {
    await router.push({ name: 'projects' })
    await router.push({ name: 'project', params: { project: 'AgentKate' } })
    await router.push({ name: 'agent', params: { threadId: 'thread / 42' } })

    expect(router.currentRoute.value.name).toBe('agent')
    expect(window.location.pathname).toBe('/agents/thread%20%2F%2042')

    const navigatedBack = new Promise((resolve) => {
      const remove = router.afterEach((to) => {
        if (to.name === 'project') { remove(); resolve() }
      })
    })
    router.back()
    await navigatedBack

    expect(router.currentRoute.value.params.project).toBe('AgentKate')
    expect(window.location.pathname).toBe('/projects/AgentKate')
  })
})
