import { describe, expect, mock, test } from 'claude-code/testing'

const DENIAL = 'Permission for this action was denied by the Claude Code auto mode classifier. Reason: Network fetch'
const BAND = {
  plugin: 'denial-prompt',
  component: 'AbovePrompt',
  props: { hasSurvey: false, isWorking: true, maxRows: 20, bodyColumns: 100, scroll: { offset: 0, bodyRows: 20 }, view: {} },
} as const

// engine stands in for core beneath the plugin: the first call of a command is
// denied by auto mode; a call carrying the user's consent runs.
function engine(on: Parameters<Parameters<typeof test>[1]>[1], seen: { consent?: string; calls: number }) {
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
    const call = $.tool.call({ tool: 'Bash', command: 'curl -sI https://example.com' })
    await clock.settle()

    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect((await ui.find({ type: 'Text', text: /Auto mode blocked: Bash: curl -sI https:\/\/example\.com/ }))).toBeDefined()
    await ui.press({ key: 'allow' })
    const r = await call
    expect(r.deny).toBeUndefined()
    expect(seen.calls).toBe(2)
    expect(seen.consent).toContain('Allow once')
    expect(seen.consent).toContain('curl -sI https://example.com')
    expect(await ui.find({ key: 'allow' })).toBeUndefined()
    await ui.unmount()
  })

  test('Deny keeps the denial and says the user declined', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const call = $.tool.call({ tool: 'Bash', command: 'rm -rf build' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    await ui.press({ key: 'deny' })
    const r = await call
    expect(r.deny).toContain(DENIAL)
    expect(r.deny).toContain('declined')
    expect(seen.calls).toBe(1)
    await ui.unmount()
  })

  test('no answer within 30 seconds keeps the denial', async ($, on) => {
    const clock = mock.clock(on)
    const seen: { consent?: string; calls: number } = { calls: 0 }
    engine(on, seen)
    const call = $.tool.call({ tool: 'Bash', command: 'git push --force' })
    await clock.settle()
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    await clock.advance(10_000)
    expect(await ui.find({ type: 'Text', text: /denies in 20s/ })).toBeDefined()
    await clock.advance(21_000)
    const r = await call
    expect(r.deny).toContain('did not answer within 30 seconds')
    expect(seen.calls).toBe(1)
    expect(await ui.find({ key: 'allow' })).toBeUndefined()
    await ui.unmount()
  })

  test('other errors pass through without a prompt', async ($, on) => {
    mock.clock(on)
    on('tool.call', { tool: 'Bash' }, async () => ({ deny: 'Permission for this action has been denied. Reason: a settings rule' }))
    const r = await $.tool.call({ tool: 'Bash', command: 'ls' })
    expect(r.deny).toBe('Permission for this action has been denied. Reason: a settings rule')
  })
})
