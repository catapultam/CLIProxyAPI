import { describe, expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

const DENIAL = 'Permission for this action was denied by the Claude Code auto mode classifier. Reason: Network fetch'
const BAND = {
  plugin: 'denial-prompt',
  component: 'AbovePrompt',
  props: { hasSurvey: false, isWorking: true, maxRows: 20, bodyColumns: 100, scroll: { offset: 0, bodyRows: 20 }, view: {} },
} as const

// engine stands in for core beneath the plugin: the first call of a command is
// denied by auto mode; a call carrying the user's consent runs.
function engine(on: On, seen: { consent?: string; calls: number }) {
  // The engine draws nothing above the prompt on its own.
  on("ui.render", { component: "AbovePrompt" }, async ($, e) => {
    const { Box } = $.ui.resolve(e)
    return <Box />
  })
  on('tool.call', { tool: 'Bash' }, async (_$, e) => {
    seen.calls += 1
    const consent = (e as { consent?: string }).consent
    if (consent === undefined) return { deny: DENIAL }
    seen.consent = consent
    return { result: { stdout: 'HTTP/1.1 200 OK', stderr: '', interrupted: false } }
  })
}

describe('denial-prompt', () => {
  test('Allow once re-runs the call with the user consent', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const call = $.tool.call({ tool: 'Bash', command: 'curl -sI https://example.com', tool_use_id: 't1' })
    await clock.settle()

    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect((await ui.find({ type: 'Text', text: /Auto mode blocked: Bash: curl -sI https:\/\/example\.com/ }))).toBeDefined()
    await ui.press({ key: 'allow:t1' })
    const r = await call
    expect(r.deny).toBeUndefined()
    expect(seen.calls).toBe(2)
    expect(seen.consent).toContain('Allow once')
    expect(seen.consent).toContain('curl -sI https://example.com')
    expect(await ui.find({ key: 'allow:t1' })).toBeUndefined()
    await ui.unmount()
  })

  test('Deny keeps the denial and says the user declined', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const call = $.tool.call({ tool: 'Bash', command: 'rm -rf build', tool_use_id: 't1' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    await ui.press({ key: 'deny:t1' })
    const r = await call
    expect(r.deny).toContain(DENIAL)
    expect(r.deny).toContain('declined')
    expect(seen.calls).toBe(1)
    await ui.unmount()
  })

  test('no answer within 8 seconds keeps the denial', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    await clock.advance(3_000)
    expect(await ui.find({ type: 'Text', text: /denies in 5s/ })).toBeDefined()
    await clock.advance(6_000)
    const r = await call
    expect(r.deny).toContain('did not answer within 8 seconds')
    expect(seen.calls).toBe(1)
    expect(await ui.find({ key: 'allow:t1' })).toBeUndefined()
    await ui.unmount()
  })

  test('other errors pass through without a prompt', async ($, on) => {
    mock.clock(on)
    on('tool.call', { tool: 'Bash' }, async () => ({ deny: 'Permission for this action has been denied. Reason: a settings rule' }))
    const r = await $.tool.call({ tool: 'Bash', command: 'ls' })
    expect(r.deny).toBe('Permission for this action has been denied. Reason: a settings rule')
  })
})


type Fetched = { method: string; url: string; body?: Record<string, unknown> }
type Reply = { status: number; text: string }

// proxy stands in for the agentbus routes: the answer to POST /prompt, and a
// wait handler that may hold a long poll open until the test releases it.
function proxy(on: On, opts: { prompt: Reply; wait: (n: number, release: Array<(r: Reply) => void>) => Reply | Promise<Reply> }) {
  mock.env(on, { ANTHROPIC_BASE_URL: 'http://bus.test:8317//', ANTHROPIC_AUTH_TOKEN: 'tok' })
  on('session.id', () => ({ value: 'sess-1' }))
  const calls: Fetched[] = []
  const release: Array<(r: Reply) => void> = []
  let waits = 0
  on('http.fetch', (_$, e) => {
    const method = e.init?.method ?? 'GET'
    calls.push({ method, url: e.url, body: e.init?.body ? JSON.parse(e.init.body) : undefined })
    const done = (r: Reply) => ({ value: { ...r, ok: r.status < 300, headers: {} } })
    if (e.url.endsWith('/v1/agentbus/prompt')) return done(opts.prompt)
    if (e.url.includes('/prompt/wait?')) {
      waits += 1
      return Promise.resolve(opts.wait(waits, release)).then(done)
    }
    if (e.url.endsWith('/prompt/resolve')) return done({ status: 200, text: '{}' })
    return done({ status: 404, text: '' })
  })
  return { calls, release, waits: () => waits }
}

const OPENED = { status: 200, text: '{"id":"p_1"}' }
const resolves = (calls: Fetched[]) => calls.filter(c => c.url.endsWith('/prompt/resolve')).map(c => c.body?.outcome)

describe('denial-prompt rows', () => {
  test('two parallel denials are answered independently', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const a = $.tool.call({ tool: 'Bash', command: 'make a', tool_use_id: 'ta' })
    const b = $.tool.call({ tool: 'Bash', command: 'make b', tool_use_id: 'tb' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect(await ui.find({ type: 'Text', text: /Bash: make a/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /Bash: make b/ })).toBeDefined()
    await ui.press({ key: 'deny:ta' })
    const ra = await a
    expect(ra.deny).toContain('declined')
    expect(await ui.find({ key: 'allow:ta' })).toBeUndefined()
    expect(await ui.find({ key: 'allow:tb' })).toBeDefined()
    await ui.press({ key: 'allow:tb' })
    const rb = await b
    expect(rb.deny).toBeUndefined()
    expect(seen.consent).toContain('make b')
    await ui.unmount()
  })

  test('a second denial gets its own full window', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const a = $.tool.call({ tool: 'Bash', command: 'first', tool_use_id: 'ta' })
    await clock.settle()
    await clock.advance(5_000)
    const b = $.tool.call({ tool: 'Bash', command: 'second', tool_use_id: 'tb' })
    await clock.settle()
    await clock.advance(3_000) // the first is out at 8 s; the second has 5 s left
    expect((await a).deny).toContain('did not answer within 8 seconds')
    await clock.advance(5_000)
    expect((await b).deny).toContain('did not answer within 8 seconds')
  })
})

describe('denial-prompt Slack path', () => {
  test('a Slack allow_once re-runs the call with consent and resolves ran', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 200, text: '{"answer":{"choice":"allow_once","by":"alex"}}' }) })
    const r = await $.tool.call({ tool: 'Bash', command: 'curl -sI https://example.com', tool_use_id: 't1' })
    await clock.settle()
    expect(r.deny).toBeUndefined()
    expect(seen.calls).toBe(2)
    expect(seen.consent).toContain('Allow once')
    const open = p.calls.find(c => c.url.endsWith('/v1/agentbus/prompt'))
    expect(open?.url).toBe('http://bus.test:8317/v1/agentbus/prompt')
    expect(open?.body).toMatchObject({ session: 'sess-1', kind: 'denial', title: 'Auto mode blocked' })
    expect(String(open?.body?.detail)).toContain('curl -sI https://example.com')
    const key = Array.from(
      new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode('denial|t1'))),
      b => b.toString(16).padStart(2, '0'),
    ).join('')
    expect(open?.body?.key).toBe(key)
    expect(resolves(p.calls)).toEqual(['ran'])
    expect(p.calls.find(c => c.url.endsWith('/prompt/resolve'))?.body).toMatchObject({ session: 'sess-1', id: 'p_1' })
  })

  test('a Slack deny keeps the denial and resolves denied', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 200, text: '{"answer":{"choice":"deny"}}' }) })
    const r = await $.tool.call({ tool: 'Bash', command: 'rm -rf build', tool_use_id: 't1' })
    await clock.settle()
    expect(r.deny).toContain('declined')
    expect(seen.calls).toBe(1)
    expect(resolves(p.calls)).toEqual(['denied'])
  })

  test('the Slack window is 10 minutes and a timeout resolves cancelled', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: (_n, release) => new Promise<Reply>(res => release.push(res)) })
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect(await ui.find({ type: 'Text', text: /denies in 10m/ })).toBeDefined()
    // Each long poll comes back empty after 25 s.
    for (let i = 0; i < 25; i++) {
      await clock.advance(25_000)
      p.release.splice(0).forEach(r => r({ status: 204, text: '' }))
      await clock.settle()
    }
    const r = await call
    expect(r.deny).toContain('did not answer within 10 minutes')
    expect(resolves(p.calls)).toEqual(['cancelled'])
    await ui.unmount()
  })

  test('a band press wins over a pending Slack wait and the loop stops', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const p = proxy(on, { prompt: OPENED, wait: (_n, release) => new Promise<Reply>(res => release.push(res)) })
    const call = $.tool.call({ tool: 'Bash', command: 'make', tool_use_id: 't1' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect(p.waits()).toBe(1)
    await ui.press({ key: 'allow:t1' })
    const r = await call
    expect(r.deny).toBeUndefined()
    expect(resolves(p.calls)).toEqual(['ran'])
    // The held poll comes back empty after the press: no further poll starts.
    p.release.splice(0).forEach(res => res({ status: 204, text: '' }))
    await clock.settle()
    await clock.advance(30_000)
    expect(p.waits()).toBe(1)
    await ui.unmount()
  })
})

describe('denial-prompt fallback', () => {
  test('when POST /prompt fails the window is 8 seconds', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: { status: 502, text: '{"error":"x"}' }, wait: () => ({ status: 204, text: '' }) })
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    await clock.advance(8_000)
    const r = await call
    expect(r.deny).toContain('did not answer within 8 seconds')
    expect(p.waits()).toBe(0)
    expect(resolves(p.calls)).toEqual([])
  })

  test('when Slack closes the prompt the row falls back to 8 seconds', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 410, text: '' }) })
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    await clock.advance(8_000)
    const r = await call
    expect(r.deny).toContain('did not answer within 8 seconds')
    expect(resolves(p.calls)).toEqual(['cancelled'])
  })
})
