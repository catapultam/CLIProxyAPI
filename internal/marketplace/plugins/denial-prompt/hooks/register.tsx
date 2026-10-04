import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, ToolCallInput, ToolCallResult } from 'claude-code'

import type { Pending } from '../types'

// When auto mode blocks a tool call, ask the user right there whether to allow
// it once. "Allow once" runs that exact call again with the user's consent
// attached, so the permission check reads it as the user's own request. "Deny",
// or no answer within 30 seconds, keeps the denial. Nothing is ever allowed
// without a press.

const TIMEOUT_SECONDS = 30

// The texts Claude Code gives the model when auto mode refuses a call: the
// classifier's denial, a check that could not decide, and a transient failure
// of the server-side check.
const AUTO_MODE_DENIAL =
  /denied by the Claude Code auto mode classifier|Auto mode could not evaluate this action|transient failure of the check/

type Choice = 'allow' | 'deny' | 'timeout'

const pending = atom<Pending | null>({ plugin: 'denial-prompt', key: 'pending' } as const, null)

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

// The resolver of the question on screen; a press or the countdown settles it.
let answer: ((choice: Choice) => void) | undefined
// Questions are asked one at a time.
let queue: Promise<unknown> = Promise.resolve()

// countdown redraws the seconds left each second, then settles as a timeout.
async function countdown($: EngineInterface, signal: AbortSignal, settled: () => boolean): Promise<void> {
  for (let left = TIMEOUT_SECONDS - 1; left >= 0 && !settled(); left--) {
    await $.clock.sleep(1000, { signal })
    if (!settled()) await update($, pending, p => (p ? { ...p, secondsLeft: left } : p))
  }
}

// ask shows the question above the prompt and resolves with the user's choice.
async function ask($: EngineInterface, summary: string, signal: AbortSignal): Promise<Choice> {
  await update($, pending, () => ({ summary, secondsLeft: TIMEOUT_SECONDS }))
  let settled = false
  try {
    return await new Promise<Choice>(resolve => {
      answer = choice => {
        if (!settled) {
          settled = true
          resolve(choice)
        }
      }
      countdown($, signal, () => settled).then(
        () => answer?.('timeout'),
        () => answer?.('timeout'),
      )
    })
  } finally {
    answer = undefined
    await update($, pending, () => null)
  }
}

export const register: Register = on => {
  on('tool.call', async ($, e, next) => {
    const r = await next(e)
    const denied = denialText(r)
    if (denied === undefined) return r

    const summary = summarize(e)
    const turn = queue.then(() => ask($, summary, next.signal))
    queue = turn.catch(() => undefined)
    const choice = await turn.catch((): Choice => 'timeout')

    if (choice !== 'allow') {
      const why =
        choice === 'deny'
          ? 'The user was asked to allow it once and declined.'
          : `The user was asked to allow it once and did not answer within ${TIMEOUT_SECONDS} seconds.`
      return { deny: `${denied}\n\n${why}` }
    }
    const { tool_use_id: _id, agentId: _agent, ...args } = e as ToolCallInput & { agentId?: string }
    return $.tool.call({
      ...(args as Parameters<typeof $.tool.call>[0]),
      consent: `The user pressed "1: Allow once" on "Auto mode blocked: ${summary}"`,
    })
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const p = await read($, pending)
    if (p === null) return next(e)
    const { Box, Text, Button } = $.ui.resolve(e)
    return (
      <Box flexDirection="column">
        <Text>Auto mode blocked: {p.summary}</Text>
        <Box>
          <Button key="allow" hotkey="1" plain label="Allow once" onPress={() => answer?.('allow')} />
          <Text> </Text>
          <Button key="deny" hotkey="2" plain label="Deny" onPress={() => answer?.('deny')} />
          <Text dimColor> · denies in {p.secondsLeft}s</Text>
        </Box>
      </Box>
    )
  })
}
