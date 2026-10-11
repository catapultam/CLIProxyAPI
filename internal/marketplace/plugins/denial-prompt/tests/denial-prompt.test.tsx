import { describe, expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { redact } from '../hooks/register'

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
function proxy(on: On, opts: { prompt: Reply; wait: (n: number, release: Array<(r: Reply) => void>) => Reply | Promise<Reply>; env?: Record<string, string> }) {
  mock.env(on, { ANTHROPIC_BASE_URL: 'http://bus.test:8317//', ANTHROPIC_AUTH_TOKEN: 'tok', ...opts.env })
  on('session.id', () => ({ value: 'sess-1' }))
  on('session.start', () => ({ cwd: 'C:/work' }))
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
type Dollar = Parameters<Parameters<typeof test>[1]>[0]
// The session says whether a person is at the prompt; Slack is used only when one is.
const start = ($: Dollar, isInteractive = true) => $.session.start({ surface: isInteractive ? 'terminal' : null, isInteractive, cwd: 'C:/work' })
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
    await start($)
    const r = await $.tool.call({ tool: 'Bash', command: 'curl -sI https://example.com', tool_use_id: 't1' })
    await clock.settle()
    expect(r.deny).toBeUndefined()
    expect(seen.calls).toBe(2)
    expect(seen.consent).toContain('Allow once')
    const open = p.calls.find(c => c.url.endsWith('/v1/agentbus/prompt'))
    expect(open?.url).toBe('http://bus.test:8317/v1/agentbus/prompt')
    expect(open?.body).toMatchObject({ session: 'sess-1', kind: 'denial', title: 'Auto mode blocked: Bash: curl -sI https://example.com' })
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
    await start($)
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
    await start($)
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
    await start($)
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
    await start($)
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
    await start($)
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    await clock.advance(8_000)
    const r = await call
    expect(r.deny).toContain('did not answer within 8 seconds')
    expect(resolves(p.calls)).toEqual(['cancelled'])
  })
})

// A denial that is answered by a re-run: the first call is refused, the call with consent gives `again`.
function engineAgain(on: On, again: object, seen: { calls: number }) {
  on('ui.render', { component: 'AbovePrompt' }, async ($, e) => {
    const { Box } = $.ui.resolve(e)
    return <Box />
  })
  on('tool.call', { tool: 'Bash' }, async (_$, e) => {
    seen.calls += 1
    return (e as { consent?: string }).consent === undefined ? { deny: DENIAL } : (again as never)
  })
}

const hotkeyOf = async (ui: Awaited<ReturnType<Dollar['ui']['mount']>>, key: string) =>
  ((await ui.find({ key })) as { props?: { hotkey?: string } } | undefined)?.props?.hotkey

describe('denial-prompt review fixes', () => {
  test('a re-run that errors ran: it is resolved ran, not denied', async ($, on) => {
    const clock = mock.clock(on)
    const seen = { calls: 0 }
    engineAgain(on, { result: { stdout: 'boom' }, isError: true }, seen)
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 200, text: '{"answer":{"choice":"allow_once"}}' }) })
    await start($)
    const r = await $.tool.call({ tool: 'Bash', command: 'make', tool_use_id: 't1' })
    await clock.settle()
    expect(r.isError).toBe(true)
    expect(seen.calls).toBe(2)
    expect(resolves(p.calls)).toEqual(['ran'])
  })

  test('a re-run that is explicitly denied is resolved denied', async ($, on) => {
    const clock = mock.clock(on)
    engineAgain(on, { deny: 'refused again' }, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 200, text: '{"answer":{"choice":"allow_once"}}' }) })
    await start($)
    const r = await $.tool.call({ tool: 'Bash', command: 'make', tool_use_id: 't1' })
    await clock.settle()
    expect(r.deny).toBe('refused again')
    expect(resolves(p.calls)).toEqual(['denied'])
  })

  test('a session that is not interactive does not use Slack: the window is 8 seconds', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 204, text: '' }) })
    await start($, false)
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    await clock.advance(8_000)
    expect((await call).deny).toContain('did not answer within 8 seconds')
    expect(p.calls.some(c => c.url.includes('/prompt'))).toBe(false)
  })

  test('AGENTBUS_SLACK_PROMPTS=0 keeps the denial off Slack', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 204, text: '' }), env: { AGENTBUS_SLACK_PROMPTS: '0' } })
    await start($)
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    await clock.advance(8_000)
    expect((await call).deny).toContain('did not answer within 8 seconds')
    expect(p.calls.some(c => c.url.includes('/prompt'))).toBe(false)
  })

  test('the Slack detail is the redacted full action, the title the 160-character summary', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 200, text: '{"answer":{"choice":"deny"}}' }) })
    await start($)
    const command = `deploy --password hunter2 ${'x'.repeat(140)} GITHUB_TOKEN=ghs_abcdefghijklmnopqrstuvwxyz ${'y'.repeat(300)}`
    await $.tool.call({ tool: 'Bash', command, tool_use_id: 't1' })
    await clock.settle()
    const body = p.calls.find(c => c.url.endsWith('/v1/agentbus/prompt'))?.body as { title: string; detail: string }
    expect(body.detail).toBe(`Bash: ${command}`.replace('hunter2', '[redacted]').replace('ghs_abcdefghijklmnopqrstuvwxyz', '[redacted]'))
    expect(body.detail.length).toBeGreaterThan(400)
    expect(body.title.startsWith('Auto mode blocked: Bash: deploy --password [redacted] ')).toBe(true)
    expect(body.title.length).toBeLessThanOrEqual('Auto mode blocked: '.length + 160)
    expect(body.title).not.toContain('hunter2')
  })

  test('a secret that straddles the 160-character cut is redacted before the cut', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const call = $.tool.call({ tool: 'Bash', command: `${'a'.repeat(120)} API_KEY=sk-abcdefghijklmnopqrstuvwxyz0123456789`, tool_use_id: 't1' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect(await ui.find({ type: 'Text', text: /sk-/ })).toBeUndefined()
    expect(await ui.find({ type: 'Text', text: /API_KEY=\[redacted\]$/ })).toBeDefined()
    await ui.press({ key: 'deny:t1' })
    await call
    await ui.unmount()
  })

  test('hotkeys only while exactly one row is open', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const a = $.tool.call({ tool: 'Bash', command: 'make a', tool_use_id: 'ta' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect(await hotkeyOf(ui, 'allow:ta')).toBe('1')
    expect(await hotkeyOf(ui, 'deny:ta')).toBe('2')
    const b = $.tool.call({ tool: 'Bash', command: 'make b', tool_use_id: 'tb' })
    await clock.settle()
    expect(await hotkeyOf(ui, 'allow:ta')).toBeUndefined()
    expect(await hotkeyOf(ui, 'allow:tb')).toBeUndefined()
    await ui.press({ key: 'deny:ta' })
    await a
    await clock.settle()
    expect(await hotkeyOf(ui, 'allow:tb')).toBe('1')
    await ui.press({ key: 'deny:tb' })
    await b
    await ui.unmount()
  })
})

describe('denial-prompt redact', () => {
  test('masks names that end in a secret word, flags and shapes; leaves code expressions alone', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 200, text: '{"answer":{"choice":"deny"}}' }), env: { ANTHROPIC_AUTH_TOKEN: 'proxy-credential-value' } })
    await start($)
    const command = [
      'ANTHROPIC_AUTH_TOKEN=x GITHUB_TOKEN=ghs_x DB_PASSWORD=hunter2 AWS_SECRET_ACCESS_KEY=x',
      '{"password": "x"} --password x --token y --token=z',
      'a ghp_abcdefghijklmnopqrstuvwxyz0 b sk-ant-api03-abcdefghijklmnopqrstu',
      'token = tokens[i] secret = getSecret() password = cfg.password pip install sk-learn',
    ].join(' ')
    await $.tool.call({ tool: 'Bash', command, tool_use_id: 't1' })
    await clock.settle()
    const body = p.calls.find(c => c.url.endsWith('/v1/agentbus/prompt'))?.body as { detail: string }
    expect(body.detail).toBe(
      'Bash: ANTHROPIC_AUTH_TOKEN=[redacted] GITHUB_TOKEN=[redacted] DB_PASSWORD=[redacted] AWS_SECRET_ACCESS_KEY=[redacted] ' +
        '{"password": [redacted]} --password [redacted] --token [redacted] --token=[redacted] ' +
        'a [redacted] b [redacted] ' +
        'token = tokens[i] secret = getSecret() password = cfg.password pip install sk-learn',
    )
  })
})

describe('denial-prompt final review fixes', () => {
  test('"still open" answers that come back at once give up after five and fall back to 8 seconds', async ($, on) => {
    const clock = mock.clock(on)
    engine(on, { calls: 0 })
    const p = proxy(on, { prompt: OPENED, wait: () => ({ status: 204, text: '' }) })
    await start($)
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force', tool_use_id: 't1' })
    await clock.settle()
    expect(p.waits()).toBe(5)
    await clock.advance(8_000)
    const r = await call
    expect(r.deny).toContain('did not answer within 8 seconds')
  })

  test('redact stays linear on long runs of dashes', () => {
    for (const text of ['-'.repeat(100_000), `${'-'.repeat(50_000)}token x`]) {
      const started = Date.now()
      redact(text)
      expect(Date.now() - started, `${text.length} chars`).toBeLessThan(500)
    }
    expect(redact('run --api-token abc123 now')).toBe('run --api-token [redacted] now')
  })
})
