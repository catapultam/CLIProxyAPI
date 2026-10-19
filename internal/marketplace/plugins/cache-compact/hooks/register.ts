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
// It does nothing in a non-interactive run.
// The least time before the cache expires at which a compaction may still start: core runs its
// hooks and builds the request first, and the request must reach the server while the cache is warm.
const MIN_LEFT_MS = 10_000
let ttlMs = 300_000
let marginMs = 30_000
let minTokens = 40_000

// The smallest context to compact: tokens, or a percent of the context window.
type Threshold = { tokens: number } | { percent: number }
let threshold: Threshold = { tokens: minTokens }

let isEnabled = true
let isInteractive = true
// cancel() bumps it, so an arm() that awaits while another starts, or a timer that a later
// arm() replaced, can tell and stops.
let generation = 0
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
  if (n <= 0) return undefined
  if (unit === '%') return n <= 100 ? { percent: n } : undefined
  const tokens = Math.round(n * (unit === 'k' ? 1e3 : unit === 'm' ? 1e6 : 1))
  return tokens >= 1 ? { tokens } : undefined
}

function describe(t: Threshold) {
  return 'percent' in t ? `${t.percent}% of the context window` : `${t.tokens.toLocaleString('en-US')} tokens`
}

function kilo(tokens: number) {
  return `${Math.round(tokens / 1000)}k`
}

function cancel() {
  generation++
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
  const g = generation
  if (!isInteractive) return
  const lastRequestAt = await read($, lastRequest)
  if (g !== generation) return
  if (!isEnabled || lastRequestAt === null) {
    const text = (await read($, isCompacted)) ? 'compacted · waiting for next reply' : 'waiting for next reply'
    if (g === generation) $.ui.status(isEnabled ? text : undefined)
    return
  }
  const expiresAt = lastRequestAt + ttlMs
  const compactAt = expiresAt - marginMs
  const { context } = await $.session.usage()
  if (g !== generation) return
  const min = 'percent' in threshold ? (context.window * threshold.percent) / 100 : threshold.tokens
  const tokens = context.tokens
  // An unknown size is never compacted, and neither is any context under a minimum below 1 token.
  const isLarge = tokens !== undefined && min >= 1 && tokens >= min
  const below = tokens === undefined ? 'no compact: size unknown' : `no compact: ${kilo(tokens)} < ${kilo(min)}`

  // A cold cache is never compacted: that would read the whole context uncached and lose
  // detail for nothing. This covers a setting changed after the TTL, and a laptop that slept.
  const lastStart = expiresAt - MIN_LEFT_MS
  const show = (now: number) => {
    if (isLarge && now < lastStart) $.ui.status(`compact in ${clock(compactAt - now)}`)
    else if (now < expiresAt) $.ui.status(`${isLarge ? 'no compact: too close to expiry' : below} · cold in ${clock(expiresAt - now)}`)
    else {
      $.ui.status(isLarge ? 'no compact: cache already cold' : `${below} · cold`)
      ticker?.cancel()
      ticker = undefined
    }
  }
  const now = await $.clock.now()
  if (g !== generation) return
  show(now)
  if (now >= expiresAt) return
  ticker = $.clock.every(1000, async () => {
    const at = await $.clock.now()
    if (g === generation) show(at)
  })

  if (!isLarge || now >= lastStart) return
  timer = $.clock.after(Math.max(0, compactAt - now), async () => {
    if (g !== generation) return
    timer = undefined
    ticker?.cancel()
    ticker = undefined
    if ((await read($, lastRequest)) === null || g !== generation) return
    // The timer can fire late (after a sleep): check the clock again.
    if ((await $.clock.now()) >= lastStart) {
      if (g === generation) $.ui.status('no compact: cache cold or too close to expiry')
      return
    }
    if (g !== generation) return

    $.ui.status('compacting')
    try {
      const done = await $.session.compact()
      // The conversation ended (/clear, /resume) or a turn started meanwhile: nothing to show here.
      if (g !== generation) return
      if ('skip' in done) {
        $.ui.status(`compaction skipped (${done.skip})`)
      } else {
        await compacted($)
        $.ui.toast('cache-compact: compacted before cache expiry')
      }
    } catch {
      // The compaction failed: a turn started meanwhile (its requests keep the cache warm), the
      // summarizer request failed, or there was too little to compact. No retry in this countdown.
      if (g === generation) $.ui.status(undefined)
    }
  })
}

export const register: Register = (on, options) => {
  // A value that makes no sense falls back to its default: a margin not inside the TTL would
  // compact right after every turn, one under MIN_LEFT_MS would never compact, and a minimum
  // under 1 token would compact any session.
  ttlMs = Number(options.ttlSeconds ?? 300) * 1000
  marginMs = Number(options.marginSeconds ?? 30) * 1000
  if (!(ttlMs > 2 * MIN_LEFT_MS)) ttlMs = 300_000
  if (!(marginMs >= MIN_LEFT_MS && marginMs < ttlMs)) marginMs = Math.min(30_000, ttlMs / 2)
  minTokens = Number(options.minTokens ?? 40000)
  if (!(minTokens >= 1)) minTokens = 40_000
  threshold = { tokens: minTokens }
  isInteractive = true

  on('session.start', async ($, e, next) => {
    isInteractive = e.isInteractive !== false
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

  // A turn with no usage (an interrupt or an API error before any request counted) did not warm
  // the cache, so the time its request started says nothing: no countdown until the next one.
  on('turn.complete', async ($, e, next) => {
    const result = await next(e)
    if (e.agentId === undefined) {
      if (e.usage === undefined) await update($, lastRequest, () => null)
      await arm($)
    }
    return result
  })

  // Another model has its own cache, cold for this conversation: no countdown until it answers.
  on('classic.PostModelSwitch', async ($, e, next) => {
    if (!e.agent_id && e.from_model !== e.to_model) {
      await update($, lastRequest, () => null)
      await arm($)
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  // /clear, /resume and the end of the session: the countdown belongs to the conversation that
  // ended, so it stops here and never compacts the next one. A /clear raises no session.start;
  // the next main turn arms again.
  on('session.end', async ($, e, next) => {
    cancel()
    await update($, lastRequest, () => null)
    await update($, isCompacted, () => false)
    $.ui.status(undefined)
    return next(e)
  }).catch(($, e, next) => next(e))
}
