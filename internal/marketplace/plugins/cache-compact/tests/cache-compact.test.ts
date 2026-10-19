import { test, expect, mock } from 'claude-code/testing'
import type { On } from 'claude-code'
import type { Engine } from 'claude-code/testing'

const MSG = [{ role: 'user', text: 'summary', toolUses: [] }] as never
const TTL = 300_000
const MARGIN = 30_000

// The test's hooks stand for the engine: count compactions, report a big context.
// tokens null: the engine does not know the context size. failStep: the step with that index gets
// no response. hold: the compaction waits for it.
const world = (on: On, tokens: number | null = 100_000, opts: { failStep?: number; hold?: Promise<void> } = {}) => {
  const clock = mock.clock(on)
  const calls: string[] = []
  const status: (string | undefined)[] = []
  on('session.compact', async ($, e) => {
    // The kit leaves `trigger` unset on a plugin's own call.
    calls.push(e.trigger ?? 'plugin')
    if (opts.hold) await opts.hold
    return { messages: MSG }
  })
  on('turn.start', ($, e) => ({ turnId: e.turnId }))
  on('turn.step', async function* ($, e) {
    const failed = e.index === opts.failStep
    // A failing step streams something first, so a test can abandon it part way.
    if (failed) yield { kind: 'text', index: 0, text: '', turnId: e.turnId, step: e.index } as never
    return { turnId: e.turnId, index: e.index, answer: '', toolUses: [], stopReason: failed ? null : 'end_turn', usage: null } as never
  })
  on('turn.complete', () => ({ text: '' }))
  on('ui.status', ($, e) => (status.push(e.text), { value: undefined }) as never)
  on('ui.toast', () => ({ value: undefined }) as never)
  on('session.usage', () => ({ value: { startedAt: 0, context: { tokens: tokens ?? undefined, window: 200_000 }, rateLimits: [] } }))
  return { clock, calls, status }
}

// What a turn whose requests counted carries; a turn that died before any did has none.
const USAGE = { input_tokens: 1, output_tokens: 1, cache_read_input_tokens: 0, cache_creation_input_tokens: 0, model: 'm' }

// One main-loop turn: one request at the current time, `runMs` long, then the turn completes.
const turn = async ($: Engine, turnId: string, clock?: { advance: (ms: number) => Promise<void> }, runMs = 0, extra: object = { reason: 'answer', usage: USAGE }) => {
  await $.turn.start({ turnId, text: 'hi' })
  const stream = $.turn.step({ turnId, index: 0, model: 'm', messageCount: 1 })
  for await (const _ of stream) void _
  if (clock && runMs > 0) await clock.advance(runMs)
  await $.turn.complete({ turnId, answer: '', durationMs: 1, isAborted: false, ...extra } as never)
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
  await $.turn.complete({ turnId: 't1', answer: '', durationMs: 1, isAborted: false, reason: 'answer', usage: USAGE } as never)
  expect(status.at(-1)).toBe('compact in 4:30')
})

test('the end of a conversation (/clear, /resume) stops its countdown', async ($, on) => {
  const { clock, calls, status } = world(on)
  on('session.end', ($, e) => ({ sessionId: e.sessionId }))
  await turn($, 't1')
  await clock.advance(60_000)
  await $.session.end({ reason: 'clear', sessionId: 's1', resume: { id: 's1' } } as never)
  expect(status.at(-1)).toBe(undefined)
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a subagent turn does not cancel or move the countdown', async ($, on) => {
  const { clock, calls } = world(on)
  await turn($, 't1')
  await clock.advance(60_000)
  await $.turn.start({ turnId: 's1', text: '', agentId: 'a1' } as never)
  for await (const _ of $.turn.step({ turnId: 's1', index: 0, model: 'm', messageCount: 1, agentId: 'a1' } as never)) void _
  await $.turn.complete({ turnId: 's1', answer: '', durationMs: 1, isAborted: false, reason: 'answer', usage: USAGE, agentId: 'a1' } as never)
  await clock.advance(TTL - MARGIN - 60_000)
  expect(calls).toEqual(['plugin'])
})

test('a non-interactive run is never compacted', async ($, on) => {
  const { clock, calls } = world(on)
  on('session.start', ($, e) => e as never)
  await $.session.start({ cwd: '/tmp', surface: null, isInteractive: false } as never)
  await turn($, 't1')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('an unknown context size is never compacted', async ($, on) => {
  const { clock, calls, status } = world(on, null)
  await turn($, 't1')
  expect(status.at(-1)).toBe('no compact: size unknown · cold in 5:00')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a threshold of 0 is refused', async ($, on) => {
  world(on)
  const run = await command($, on)
  expect(await run('threshold 0')).toMatch(/^Usage:/)
  expect(await run('threshold 0%')).toMatch(/^Usage:/)
  expect(await run('threshold 0.4')).toMatch(/^Usage:/)
})

test('options that make no sense fall back to the defaults', { options: { ttlSeconds: 300, marginSeconds: 400, minTokens: 0.4 } }, async ($, on) => {
  const { clock, calls, status } = world(on, 5_000)
  await turn($, 't1')
  // The minimum is 40k again, so 5k is left alone.
  expect(status.at(-1)).toBe('no compact: 5k < 40k · cold in 5:00')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a margin outside the TTL does not compact right after the turn', { options: { ttlSeconds: 300, marginSeconds: 400, minTokens: 40000 } }, async ($, on) => {
  const { clock, calls } = world(on)
  await turn($, 't1')
  await clock.advance(1_000)
  expect(calls).toEqual([])
  await clock.advance(TTL - MARGIN - 1_000)
  expect(calls).toEqual(['plugin'])
})

test('two settings changes at once leave one countdown and compact once', async ($, on) => {
  const { clock, calls } = world(on)
  const run = await command($, on)
  await turn($, 't1')
  await Promise.all([run('on'), run('threshold 50k')])
  await clock.advance(TTL - MARGIN)
  expect(calls).toEqual(['plugin'])
  await clock.advance(TTL * 2)
  expect(calls).toEqual(['plugin'])
})

test('a lower threshold set after the cache went cold does not compact', async ($, on) => {
  const { clock, calls, status } = world(on, 30_000)
  const run = await command($, on)
  await turn($, 't1')
  await clock.advance(TTL + 1_000)
  await run('threshold 20k')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
  expect(status.at(-1)).toBe('no compact: cache already cold')
})

test('turning it on after the cache went cold does not compact', async ($, on) => {
  const { clock, calls } = world(on)
  const run = await command($, on)
  await run('off')
  await turn($, 't1')
  await clock.advance(15 * 60_000)
  await run('on')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a turn that ended on an API error before any request counted starts no countdown', async ($, on) => {
  const { clock, calls, status } = world(on)
  await turn($, 't1', clock, 0, { reason: 'error' })
  expect(status.at(-1)).toBe('waiting for next reply')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a last request that ran until close to expiry does not compact', async ($, on) => {
  const { clock, calls, status } = world(on)
  await turn($, 't1', clock, TTL - 5_000)
  expect(status.at(-1)).toBe('no compact: too close to expiry · cold in 0:05')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a lower threshold set seconds before expiry does not compact', async ($, on) => {
  const { clock, calls } = world(on, 30_000)
  const run = await command($, on)
  await turn($, 't1')
  await clock.advance(TTL - 5_000)
  await run('threshold 20k')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a margin under 10 s falls back to the default', { options: { ttlSeconds: 300, marginSeconds: 0, minTokens: 40000 } }, async ($, on) => {
  const { clock, calls } = world(on)
  await turn($, 't1')
  await clock.advance(TTL - MARGIN)
  expect(calls).toEqual(['plugin'])
})

test('a switch to another model while idle stops the countdown', async ($, on) => {
  const { clock, calls, status } = world(on)
  on('classic.PostModelSwitch', () => ({}) as never)
  await turn($, 't1')
  await clock.advance(60_000)
  await $.classic.PostModelSwitch({ from_model: 'opus', to_model: 'sonnet', requested_model: 'sonnet', source: 'command', context_tokens: 100_000, prompt_cache_warm: true, cache_ttl: '5m' } as never)
  expect(status.at(-1)).toBe('waiting for next reply')
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
})

test('a last request that got no response does not restart the countdown', async ($, on) => {
  const { clock, calls } = world(on, 100_000, { failStep: 1 })
  // Step 0 answers at 0 and asks for a tool that runs 2 minutes; step 1 at 120 s gets no response.
  await $.turn.start({ turnId: 't1', text: 'hi' })
  for await (const _ of $.turn.step({ turnId: 't1', index: 0, model: 'm', messageCount: 1 })) void _
  await clock.advance(120_000)
  for await (const _ of $.turn.step({ turnId: 't1', index: 1, model: 'm', messageCount: 1 })) void _
  await $.turn.complete({ turnId: 't1', answer: '', durationMs: 1, isAborted: false, reason: 'error', usage: USAGE } as never)
  // Counted from step 0: compact at 270 s, never at 390 s.
  await clock.advance(TTL - MARGIN - 120_000)
  expect(calls).toEqual(['plugin'])
  await clock.advance(TTL * 2)
  expect(calls).toEqual(['plugin'])
})

test('a setting changed while the compaction runs does not compact again', async ($, on) => {
  let release = () => {}
  const hold = new Promise<void>(resolve => (release = resolve))
  const { clock, calls } = world(on, 100_000, { hold })
  const run = await command($, on)
  await turn($, 't1')
  await clock.advance(TTL - MARGIN)
  expect(calls).toEqual(['plugin'])
  await run('threshold 50k')
  release()
  await clock.advance(TTL * 2)
  expect(calls).toEqual(['plugin'])
})

test('an interrupted step whose time is put back after the turn ended still counts from the earlier request', async ($, on) => {
  const { clock, calls } = world(on, 100_000, { failStep: 1 })
  await $.turn.start({ turnId: 't1', text: 'hi' })
  for await (const _ of $.turn.step({ turnId: 't1', index: 0, model: 'm', messageCount: 1 })) void _
  await clock.advance(120_000)
  // The person interrupts step 1 part way: the turn ends (and arms) before the abandoned stream puts
  // the earlier request time back.
  const step1 = $.turn.step({ turnId: 't1', index: 1, model: 'm', messageCount: 1 })[Symbol.asyncIterator]()
  await step1.next()
  await $.turn.complete({ turnId: 't1', answer: '', durationMs: 1, isAborted: true, reason: 'aborted', usage: USAGE } as never)
  await step1.return?.(undefined)
  // Counted from step 0, a compaction is due by 270 s; any later one would hit a cold cache.
  await clock.advance(TTL - MARGIN - 120_000)
  const byDeadline = [...calls]
  await clock.advance(TTL * 3)
  expect(calls).toEqual(byDeadline)
})

test('after an interrupted step soon after the last answer, the compaction still comes before expiry', async ($, on) => {
  const { clock, calls } = world(on, 100_000, { failStep: 1 })
  await $.turn.start({ turnId: 't1', text: 'hi' })
  for await (const _ of $.turn.step({ turnId: 't1', index: 0, model: 'm', messageCount: 1 })) void _
  await clock.advance(10_000)
  const step1 = $.turn.step({ turnId: 't1', index: 1, model: 'm', messageCount: 1 })[Symbol.asyncIterator]()
  await step1.next()
  await $.turn.complete({ turnId: 't1', answer: '', durationMs: 1, isAborted: true, reason: 'aborted', usage: USAGE } as never)
  await step1.return?.(undefined)
  // Armed from step 1 (fires at 280 s), then counted again from step 0: 280 s is still 20 s before
  // step 0's cache expires, so it compacts then, once.
  await clock.advance(TTL - MARGIN - 1)
  expect(calls).toEqual([])
  await clock.advance(1)
  expect(calls).toEqual(['plugin'])
  await clock.advance(TTL * 3)
  expect(calls).toEqual(['plugin'])
})

test('an interrupted first step leaves no countdown once the timer finds the time cleared', async ($, on) => {
  const { clock, calls, status } = world(on, 100_000, { failStep: 0 })
  await $.turn.start({ turnId: 't1', text: 'hi' })
  const step0 = $.turn.step({ turnId: 't1', index: 0, model: 'm', messageCount: 1 })[Symbol.asyncIterator]()
  await step0.next()
  await $.turn.complete({ turnId: 't1', answer: '', durationMs: 1, isAborted: true, reason: 'aborted', usage: USAGE } as never)
  await step0.return?.(undefined)
  await clock.advance(TTL * 2)
  expect(calls).toEqual([])
  expect(status.at(-1)).toBe('waiting for next reply')
})

