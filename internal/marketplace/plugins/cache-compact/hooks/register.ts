import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

// The cache TTL starts again at each main-loop model request. When the
// session stays idle, compact while the cache is still warm: the compaction
// reads the cached prefix, and the next prompt re-caches only the summary.
// After any compaction, wait for a new model request before the timer arms
// again, so that a compaction never follows a compaction.
// The status line shows the time left until the compaction, or until the
// cache expires when the context is too small to compact. After a
// compaction, it says so until the next model request.
// `/cache-compact on|off|threshold <value>` sets it; the settings are kept
// across sessions. The last request time is session state, so that a reload
// resumes the countdown. The compacted flag is session state too, so that the
// status line still says "compacted" after a reload.
let ttlMs = 300_000
let marginMs = 30_000
let minTokens = 40_000

// The smallest context to compact: tokens, or a percent of the context window.
type Threshold = { tokens: number } | { percent: number }
let threshold: Threshold = { tokens: minTokens }

let isEnabled = true
const lastRequest = atom({ plugin: 'cache-compact', key: 'lastRequestAt' } as const, null)
const isCompacted = atom({ plugin: 'cache-compact', key: 'isCompacted' } as const, false)
let timer: Timer | undefined
let ticker: Timer | undefined

function clock(ms: number) {
  const s = Math.max(0, Math.ceil(ms / 1000))
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

function parseThreshold(text: string): Threshold | undefined {
  const m = /^(\d+(?:\.\d+)?)\s*(k|m|%)?$/i.exec(text.trim())
  if (!m) return undefined
  const n = Number(m[1])
  const unit = m[2]?.toLowerCase()
  if (unit === '%') return n <= 100 ? { percent: n } : undefined
  return { tokens: Math.round(n * (unit === 'k' ? 1e3 : unit === 'm' ? 1e6 : 1)) }
}

function describe(t: Threshold) {
  return 'percent' in t ? `${t.percent}% of the context window` : `${t.tokens.toLocaleString('en-US')} tokens`
}

function kilo(tokens: number) {
  return `${Math.round(tokens / 1000)}k`
}

function cancel() {
  timer?.cancel()
  timer = undefined
  ticker?.cancel()
  ticker = undefined
}

async function compacted($: EngineInterface) {
  cancel()
  await update($, lastRequest, () => null)
  await update($, isCompacted, () => true)
  if (isEnabled) $.ui.status('compacted · waiting for next reply')
}

async function arm($: EngineInterface) {
  cancel()
  const lastRequestAt = await read($, lastRequest)
  if (!isEnabled || lastRequestAt === null) {
    const text = (await read($, isCompacted)) ? 'compacted · waiting for next reply' : 'waiting for next reply'
    $.ui.status(isEnabled ? text : undefined)
    return
  }
  const expiresAt = lastRequestAt + ttlMs
  const compactAt = expiresAt - marginMs
  const { context } = await $.session.usage()
  const min = 'percent' in threshold ? (context.window * threshold.percent) / 100 : threshold.tokens
  const tokens = context.tokens ?? 0
  const willCompact = tokens >= min
  const below = `no compact: ${kilo(tokens)} < ${kilo(min)}`

  const show = (now: number) => {
    if (willCompact) $.ui.status(`compact in ${clock(compactAt - now)}`)
    else if (now < expiresAt) $.ui.status(`${below} · cold in ${clock(expiresAt - now)}`)
    else {
      $.ui.status(`${below} · cold`)
      ticker?.cancel()
      ticker = undefined
    }
  }
  show(await $.clock.now())
  ticker = $.clock.every(1000, async () => show(await $.clock.now()))

  if (!willCompact) return
  timer = $.clock.after(Math.max(0, compactAt - (await $.clock.now())), async () => {
    timer = undefined
    ticker?.cancel()
    ticker = undefined
    if ((await read($, lastRequest)) === null) return

    $.ui.status('compacting')
    try {
      const done = await $.session.compact()
      if ('skip' in done) {
        $.ui.status(`compaction skipped (${done.skip})`)
      } else {
        await compacted($)
        $.ui.toast('cache-compact: compacted before cache expiry')
      }
    } catch {
      // A turn started meanwhile; its own requests keep the cache warm.
      $.ui.status(undefined)
    }
  })
}

export const register: Register = (on, options) => {
  ttlMs = Number(options.ttlSeconds ?? 300) * 1000
  marginMs = Number(options.marginSeconds ?? 30) * 1000
  minTokens = Number(options.minTokens ?? 40000)
  threshold = { tokens: minTokens }

  on('session.start', async ($, e, next) => {
    isEnabled = (await $.store.get('enabled')) !== false
    const saved = await $.store.get('threshold')
    if (typeof saved === 'string') threshold = parseThreshold(saved) ?? threshold
    await $.command.register({
      name: 'cache-compact',
      description: 'Compaction before cache expiry: /cache-compact on|off|threshold <100k|100000|10%>',
    })
    await arm($)
    return next(e)
  })

  on('command.run', { command: 'cache-compact' }, async ($, e) => {
    const [verb = '', ...rest] = e.args.trim().split(/\s+/)
    const value = rest.join(' ')
    const usage = 'Usage: /cache-compact on|off|threshold <100k|100000|10%>'

    switch (verb.toLowerCase()) {
      case '':
        return { text: `cache-compact is ${isEnabled ? 'on' : 'off'}; threshold ${describe(threshold)}.` }
      case 'on':
      case 'off':
        if (value !== '') return { text: usage }
        isEnabled = verb.toLowerCase() === 'on'
        await $.store.set('enabled', isEnabled)
        break
      case 'threshold': {
        const parsed = parseThreshold(value)
        if (!parsed) return { text: usage }
        threshold = parsed
        await $.store.set('threshold', value)
        break
      }
      default:
        return { text: usage }
    }

    await arm($)
    return { text: `cache-compact is ${isEnabled ? 'on' : 'off'}; threshold ${describe(threshold)}.` }
  })

  // Record when the request starts, not when it ends: this is the safe side.
  on('turn.step', async function* ($, e, next) {
    if (e.agentId === undefined) {
      const now = await $.clock.now()
      await update($, lastRequest, () => now)
      await update($, isCompacted, () => false)
      // An auto compaction in the turn set the "compacted" status; clear it.
      $.ui.status(undefined)
    }
    return yield* next(e)
  })

  on('turn.start', ($, e, next) => {
    if (e.agentId === undefined) {
      cancel()
      $.ui.status(undefined)
    }
    return next(e)
  })

  // Any compaction of the main conversation: /compact, auto, or a plugin.
  on('session.compact', async ($, e, next) => {
    const result = await next(e)
    if (e.agentId === undefined && e.trigger !== 'precompute' && !('skip' in result)) {
      await compacted($)
    }
    return result
  }).catch(($, e, next) => next(e))

  on('turn.complete', async ($, e, next) => {
    const result = await next(e)
    if (e.agentId === undefined) await arm($)
    return result
  })
}
