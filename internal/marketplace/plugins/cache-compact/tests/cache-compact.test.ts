import { test, expect, mock } from 'claude-code/testing'
import type { On } from 'claude-code'
import type { Engine } from 'claude-code/testing'

const MSG = [{ role: 'user', text: 'summary', toolUses: [] }] as never
const TTL = 300_000
const MARGIN = 30_000

// The test's hooks stand for the engine: count compactions, report a big context.
const world = (on: On, tokens = 100_000) => {
  const clock = mock.clock(on)
  const calls: string[] = []
  const status: (string | undefined)[] = []
  on('session.compact', ($, e) => {
    // The kit leaves `trigger` unset on a plugin's own call.
    calls.push(e.trigger ?? 'plugin')
    return { messages: MSG }
  })
  on('turn.start', ($, e) => ({ turnId: e.turnId }))
  on('turn.step', async function* ($, e) {
    return { turnId: e.turnId, index: e.index, answer: '', toolUses: [], stopReason: 'end_turn', usage: null } as never
  })
  on('turn.complete', () => ({ text: '' }))
  on('ui.status', ($, e) => (status.push(e.text), { value: undefined }) as never)
  on('ui.toast', () => ({ value: undefined }) as never)
  on('session.usage', () => ({ value: { startedAt: 0, context: { tokens, window: 200_000 }, rateLimits: [] } }))
  return { clock, calls, status }
}

const turn = async ($: Engine, turnId: string) => {
  await $.turn.start({ turnId, text: 'hi' })
  const stream = $.turn.step({ turnId, index: 0, model: 'm', messageCount: 1 })
  for await (const _ of stream) void _
  await $.turn.complete({ turnId, answer: '', durationMs: 1, isAborted: false, reason: 'answer' } as never)
}

test('compacts once, just before the TTL ends', async ($, on) => {
  const { clock, calls } = world(on)
  await turn($, 't1')
  await clock.advance(TTL - MARGIN - 1)
  expect(calls).toEqual([])
  await clock.advance(1)
  expect(calls).toEqual(['plugin'])
  await clock.advance(TTL * 4)
  expect(calls).toEqual(['plugin'])
})

test('a new turn moves the deadline', async ($, on) => {
  const { clock, calls } = world(on)
  await turn($, 't1')
  await clock.advance(TTL - MARGIN - 10_000)
  await turn($, 't2')
  await clock.advance(10_000)
  expect(calls).toEqual([])
  await clock.advance(TTL)
  expect(calls).toEqual(['plugin'])
})

test('no compaction after a manual /compact', async ($, on) => {
  const { clock, calls } = world(on)
  await turn($, 't1')
  await clock.advance(10_000)
  await $.session.compact({ trigger: 'manual', messages: MSG })
  await clock.advance(TTL * 2)
  expect(calls).toEqual(['manual'])
})

test('small context is left alone', async ($, on) => {
  const { clock, calls } = world(on, 5_000)
  await turn($, 't1')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('the status line counts down', async ($, on) => {
  const { clock, status } = world(on)
  await turn($, 't1')
  expect(status.at(-1)).toBe('compact in 4:30')
  await clock.advance(61_000)
  expect(status.at(-1)).toBe('compact in 3:29')
})

test('small context counts down to cold', async ($, on) => {
  const { clock, status } = world(on, 5_000)
  await turn($, 't1')
  expect(status.at(-1)).toBe('no compact: 5k < 40k · cold in 5:00')
  await clock.advance(TTL)
  expect(status.at(-1)).toBe('no compact: 5k < 40k · cold')
})

const command = async ($: Engine, on: On) => {
  mock.store(on)
  on('command.register', () => ({ value: undefined }) as never)
  on('session.start', ($, e) => e as never)
  await $.session.start({ cwd: '/tmp', surface: 'terminal', isInteractive: true } as never)
  return async (args: string) => (await $.command.run({ command: 'cache-compact', args } as never)).text
}

test('/cache-compact off stops it, on starts it again', async ($, on) => {
  const { clock, calls, status } = world(on)
  const run = await command($, on)
  await turn($, 't1')

  expect(await run('off')).toBe('cache-compact is off; threshold 40,000 tokens.')
  expect(status.at(-1)).toBe(undefined)
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])

  await turn($, 't2')
  expect(status.at(-1)).toBe(undefined)
  expect(await run('on')).toBe('cache-compact is on; threshold 40,000 tokens.')
  expect(status.at(-1)).toBe('compact in 4:30')
  expect(await run('maybe')).toMatch(/^Usage:/)
})

test('/cache-compact threshold takes tokens, k and percent', async ($, on) => {
  const { status } = world(on, 100_000)
  const run = await command($, on)
  await turn($, 't1')

  expect(await run('threshold 150k')).toBe('cache-compact is on; threshold 150,000 tokens.')
  expect(status.at(-1)).toBe('no compact: 100k < 150k · cold in 5:00')
  expect(await run('threshold 100000')).toBe('cache-compact is on; threshold 100,000 tokens.')
  expect(status.at(-1)).toBe('compact in 4:30')
  // The window is 200k: 60% is 120k, above the 100k context.
  expect(await run('threshold 60%')).toBe('cache-compact is on; threshold 60% of the context window.')
  expect(status.at(-1)).toBe('no compact: 100k < 120k · cold in 5:00')
  expect(await run('threshold lots')).toMatch(/^Usage:/)
  expect(await run('')).toBe('cache-compact is on; threshold 60% of the context window.')
})

test('a reload resumes the countdown and a new threshold applies at once', async ($, on) => {
  const { clock, calls, status } = world(on, 100_000)
  const run = await command($, on)
  expect(await run('threshold 200k')).toBe('cache-compact is on; threshold 200,000 tokens.')
  await turn($, 't1')
  expect(status.at(-1)).toBe('no compact: 100k < 200k · cold in 5:00')

  // A reload runs session.start again; the request time is in session state.
  await clock.advance(60_000)
  await $.session.start({ cwd: '/tmp', surface: 'terminal', isInteractive: true } as never)
  expect(status.at(-1)).toBe('no compact: 100k < 200k · cold in 4:00')

  expect(await run('threshold 50k')).toBe('cache-compact is on; threshold 50,000 tokens.')
  expect(status.at(-1)).toBe('compact in 3:30')
  await clock.advance(210_000)
  expect(calls).toEqual(['plugin'])
})

test('with no request yet, the status line says it waits', async ($, on) => {
  const { status } = world(on)
  const run = await command($, on)
  expect(status.at(-1)).toBe('waiting for next reply')
  await run('off')
  expect(status.at(-1)).toBe(undefined)
})

test('after a compaction, the status line says so until the next request', async ($, on) => {
  const { clock, status } = world(on)
  const run = await command($, on)
  await turn($, 't1')
  await clock.advance(TTL - MARGIN)
  expect(status.at(-1)).toBe('compacted · waiting for next reply')

  // A reload keeps it.
  await $.session.start({ cwd: '/tmp', surface: 'terminal', isInteractive: true } as never)
  expect(status.at(-1)).toBe('compacted · waiting for next reply')

  await turn($, 't2')
  expect(status.at(-1)).toBe('compact in 4:30')
  await $.session.compact({ trigger: 'manual', messages: MSG })
  expect(status.at(-1)).toBe('compacted · waiting for next reply')
  await run('off')
  expect(status.at(-1)).toBe(undefined)
})

test('an auto compaction in a turn shows until the next request', async ($, on) => {
  const { status } = world(on)
  await $.turn.start({ turnId: 't1', text: 'hi' })
  for await (const _ of $.turn.step({ turnId: 't1', index: 0, model: 'm', messageCount: 1 })) void _
  await $.session.compact({ trigger: 'auto', messages: MSG })
  expect(status.at(-1)).toBe('compacted · waiting for next reply')
  for await (const _ of $.turn.step({ turnId: 't1', index: 1, model: 'm', messageCount: 1 })) void _
  expect(status.at(-1)).toBe(undefined)
  await $.turn.complete({ turnId: 't1', answer: '', durationMs: 1, isAborted: false, reason: 'answer' } as never)
  expect(status.at(-1)).toBe('compact in 4:30')
})
