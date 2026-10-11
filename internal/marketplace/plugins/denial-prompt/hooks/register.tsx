import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, ToolCallInput, ToolCallResult } from 'claude-code'

// When auto mode blocks a tool call, ask the user right there whether to allow
// it once. "Allow once" runs that exact call again with the user's consent
// attached, so the permission check reads it as the user's own request. "Deny",
// or no answer in time, keeps the denial. Nothing is ever allowed without a
// press. Every denial has its own row and its own countdown.
//
// A hook has 10 s of its own time: time awaiting a plain promise or a $.clock
// wait counts, time inside a $ call (a fetch) or next(e) does not. So without
// Slack the window is 8 s (under the budget). When the agentbus proxy takes the
// question to Slack, the window is 10 minutes, and the hook waits only inside
// GET /prompt/wait fetches, never on $.clock.

const LOCAL_SECONDS = 8
const SLACK_SECONDS = 600

// The texts Claude Code gives the model when auto mode refuses a call: the
// classifier's denial, a check that could not decide, and a transient failure
// of the server-side check.
const AUTO_MODE_DENIAL =
  /denied by the Claude Code auto mode classifier|Auto mode could not evaluate this action|transient failure of the check/

type Choice = 'allow' | 'deny' | 'timeout'

const pending = atom({ plugin: 'denial-prompt', key: 'pending' } as const, {})

// denialText returns the refusal text when the result is an auto-mode denial.
export function denialText(r: ToolCallResult): string | undefined {
  const text = typeof r.deny === 'string' ? r.deny : r.isError === true ? r.text : undefined
  return text !== undefined && AUTO_MODE_DENIAL.test(text) ? text : undefined
}

// summarize names the blocked action in one short line.
export function summarize(e: ToolCallInput): string {
  const args = e as unknown as Record<string, unknown>
  const detail =
    typeof args.command === 'string'
      ? args.command
      : typeof args.file_path === 'string'
        ? args.file_path
        : typeof args.url === 'string'
          ? args.url
          : Object.values(args).find((v): v is string => typeof v === 'string' && v !== e.tool && v !== e.tool_use_id)
  const line = `${e.tool}${detail ? `: ${detail.replace(/\s+/g, ' ').trim()}` : ''}`
  return line.length > 160 ? `${line.slice(0, 159)}…` : line
}

// Each row's resolver; a band press, a Slack answer or the countdown settles it.
const answers = new Map<string, (choice: Choice) => void>()
let nextRow = 0

function windowText(seconds: number): string {
  return seconds >= 60 && seconds % 60 === 0 ? `${seconds / 60} minutes` : `${seconds} seconds`
}

function left(secondsLeft: number): string {
  return secondsLeft >= 60 ? `${Math.ceil(secondsLeft / 60)}m` : `${secondsLeft}s`
}

async function sha256Hex(text: string): Promise<string> {
  const d = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text))
  return Array.from(new Uint8Array(d), b => b.toString(16).padStart(2, '0')).join('')
}

type Reply = { status: number; json: Record<string, unknown> | null } | null
type Bus = { post(path: string, body: object): Promise<Reply>; get(path: string): Promise<Reply> }

// bus builds the agentbus client from the same env agentbus reads, or
// undefined when the proxy is not configured. Its calls never throw.
async function bus($: EngineInterface): Promise<Bus | undefined> {
  try {
    const base = ((await $.env.get('ANTHROPIC_BASE_URL')) ?? '').replace(/\/+$/, '')
    const token = (await $.env.get('ANTHROPIC_AUTH_TOKEN')) ?? ''
    if (!base || !token) return undefined
    const call = async (method: string, path: string, body?: object): Promise<Reply> => {
      try {
        const res = await $.http.fetch(`${base}/v1/agentbus${path}`, {
          method,
          headers: body
            ? { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }
            : { Authorization: `Bearer ${token}` },
          body: body ? JSON.stringify(body) : undefined,
        })
        let json: Record<string, unknown> | null = null
        try {
          json = res.text ? JSON.parse(res.text) : null
        } catch {
          json = null
        }
        return { status: res.status, json }
      } catch {
        return null
      }
    }
    return { post: (p, b) => call('POST', p, b), get: p => call('GET', p) }
  } catch {
    return undefined
  }
}

type Slack = { bus: Bus; session: string; id: string }

// openSlack posts the denial to the proxy; undefined when prompts are unavailable.
async function openSlack($: EngineInterface, toolUseId: string, summary: string): Promise<Slack | undefined> {
  try {
    const b = await bus($)
    if (!b) return undefined
    const session = await $.session.id()
    if (!session) return undefined
    const res = await b.post('/prompt', {
      session,
      key: await sha256Hex(`denial|${toolUseId}`),
      kind: 'denial',
      title: 'Auto mode blocked',
      detail: summary,
    })
    const id = res?.status === 200 ? res.json?.id : undefined
    return typeof id === 'string' && id ? { bus: b, session, id } : undefined
  } catch {
    return undefined
  }
}

// slackWait long-polls the answer, one fetch per iteration and no $.clock wait,
// until the deadline, a closed prompt, repeated errors, or stop. It returns the
// choice, or 'lost' when Slack can no longer answer.
async function slackWait(
  $: EngineInterface,
  s: Slack,
  deadline: number,
  stop: { value: boolean },
  show: (secondsLeft: number) => Promise<void>,
): Promise<Choice | 'lost'> {
  let errors = 0
  const url = `/prompt/wait?session=${encodeURIComponent(s.session)}&id=${encodeURIComponent(s.id)}`
  while (!stop.value) {
    if (deadline - (await $.clock.now()) <= 0) return 'timeout'
    const res = await s.bus.get(url)
    if (stop.value) return 'lost'
    if (res === null || res.status >= 500) {
      if (++errors >= 3) return 'lost'
      continue
    }
    errors = 0
    if (res.status === 200) {
      const a = res.json?.answer as { choice?: unknown } | undefined
      return a?.choice === 'allow_once' ? 'allow' : 'deny'
    }
    if (res.status !== 204) return 'lost' // 404, 410, anything else: Slack is gone
    await show(Math.max(0, Math.ceil((deadline - (await $.clock.now())) / 1000)))
  }
  return 'lost'
}

// countdown redraws the seconds left each second, then settles as a timeout.
async function countdown(
  $: EngineInterface,
  seconds: number,
  signal: AbortSignal,
  settled: () => boolean,
  show: (secondsLeft: number) => Promise<void>,
): Promise<Choice> {
  for (let n = seconds - 1; n >= 0 && !settled(); n--) {
    await $.clock.sleep(1000, { signal })
    if (!settled()) await show(n)
  }
  return 'timeout'
}

type Decision = { choice: Choice; window: number; slack?: Slack }

// ask shows the question above the prompt on its own row and resolves with the
// user's choice, from the band or from Slack.
async function ask($: EngineInterface, rowId: string, summary: string, signal: AbortSignal): Promise<Decision> {
  const slack = await openSlack($, rowId, summary)
  let window = slack ? SLACK_SECONDS : LOCAL_SECONDS
  const show = async (secondsLeft: number) => {
    await update($, pending, p => (rowId in p ? { ...p, [rowId]: { summary, secondsLeft } } : p))
  }
  await update($, pending, p => ({ ...p, [rowId]: { summary, secondsLeft: window } }))
  let settled = false
  const stop = { value: false }
  const local = new AbortController()
  const onAbort = () => local.abort()
  signal.addEventListener('abort', onAbort)
  try {
    const press = new Promise<Choice>(resolve => {
      answers.set(rowId, choice => {
        if (!settled) {
          settled = true
          resolve(choice)
        }
      })
    })
    const safe = <T,>(p: Promise<T>, fallback: T) => p.catch(() => fallback)
    const shortWindow = async (): Promise<Choice> =>
      Promise.race([press, safe(countdown($, window, local.signal, () => settled, show), 'timeout' as Choice)])

    let choice: Choice
    if (slack) {
      const deadline = (await $.clock.now()) + SLACK_SECONDS * 1000
      const first = await Promise.race([press, safe<Choice | 'lost'>(slackWait($, slack, deadline, stop, show), 'lost')])
      if (first === 'lost' && !settled) {
        // Slack gave up (closed, errors): finish on the short, budget-safe window.
        window = LOCAL_SECONDS
        await show(window)
        choice = await shortWindow()
      } else choice = first === 'lost' ? 'timeout' : first
    } else {
      choice = await shortWindow()
    }
    return { choice, window, slack }
  } finally {
    settled = true
    stop.value = true
    local.abort()
    signal.removeEventListener('abort', onAbort)
    answers.delete(rowId)
    await update($, pending, p => {
      const { [rowId]: _gone, ...rest } = p
      return rest
    })
  }
}

async function resolveSlack(s: Slack | undefined, outcome: 'ran' | 'denied' | 'cancelled'): Promise<void> {
  if (!s) return
  await s.bus.post('/prompt/resolve', { session: s.session, id: s.id, outcome })
}

export const register: Register = on => {
  on('tool.call', async ($, e, next) => {
    const r = await next(e)
    const denied = denialText(r)
    if (denied === undefined) return r

    const summary = summarize(e)
    const rowId = e.tool_use_id ?? `denial-${++nextRow}`
    const d = await ask($, rowId, summary, next.signal)
    const { choice, slack } = d

    if (choice !== 'allow') {
      await resolveSlack(slack, choice === 'deny' ? 'denied' : 'cancelled')
      const why =
        choice === 'deny'
          ? 'The user was asked to allow it once and declined.'
          : `The user was asked to allow it once and did not answer within ${windowText(d.window)}.`
      return { deny: `${denied}\n\n${why}` }
    }
    const { tool_use_id: _id, agentId: _agent, ...args } = e as ToolCallInput & { agentId?: string }
    try {
      const out = await $.tool.call({
        ...(args as Parameters<typeof $.tool.call>[0]),
        consent: `The user pressed "1: Allow once" on "Auto mode blocked: ${summary}"`,
      })
      await resolveSlack(slack, out.deny !== undefined || out.isError === true ? 'denied' : 'ran')
      return out
    } catch (err) {
      await resolveSlack(slack, 'cancelled')
      throw err
    }
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const rows = Object.entries(await read($, pending))
    if (rows.length === 0) return next(e)
    const { Box, Text, Button } = $.ui.resolve(e)
    return (
      <Box flexDirection="column">
        {rows.map(([id, p], i) => (
          <Box key={`row:${id}`} flexDirection="column">
            <Text>Auto mode blocked: {p.summary}</Text>
            <Box>
              <Button key={`allow:${id}`} hotkey={i === 0 ? '1' : undefined} plain label="Allow once" onPress={() => answers.get(id)?.('allow')} />
              <Text> </Text>
              <Button key={`deny:${id}`} hotkey={i === 0 ? '2' : undefined} plain label="Deny" onPress={() => answers.get(id)?.('deny')} />
              <Text dimColor> · denies in {left(p.secondsLeft)}</Text>
            </Box>
          </Box>
        ))}
      </Box>
    )
  })
}
