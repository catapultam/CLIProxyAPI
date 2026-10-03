import { expect, mock, test } from 'claude-code/testing'

const ENV = {
  ANTHROPIC_BASE_URL: 'http://bus.test:8317/',
  ANTHROPIC_AUTH_TOKEN: 'test-token',
  COMPUTERNAME: 'CPLT-4A',
}

const PEERS = [
  { address: 'cplt-4a/comms-3a9e9c', machine: 'cplt-4a', status: 'busy' },
  { address: 'cplt-4a/other-111111', machine: 'CPLT-4A', status: 'idle' },
  { address: 'shoggoth/art-0f7de4', name: 'vm-shoggoth', machine: 'shoggoth', status: 'idle' },
  { address: 'shoggoth/old-459b94', machine: 'shoggoth', status: 'offline' },
  { address: 'unknown/session-7ea579', machine: 'unknown', status: 'away' },
]

type Call = { url: string; method: string; body: unknown; auth: string | undefined }

const DEFAULT_SESSION_ID = '3a9e9c5c-0000-0000-0000-000000000000'

// session is a mutable box so a test can change what $.session.id() answers
// mid-test (simulating /clear, /resume, or /branch), the way wire()'s other
// stubs are fixed at setup time.
function wire(
  $: unknown,
  on: Parameters<Parameters<typeof test>[1]>[1],
  waits: Array<{ status: number; text: string }>,
  session: { id: string } = { id: DEFAULT_SESSION_ID },
) {
  const calls: Call[] = []
  mock.env(on, ENV)
  on('session.id', () => ({ value: session.id }))
  on('session.start', () => ({ cwd: 'C:/work/comms' }))
  on('session.end', ($, e) => ({ sessionId: e.sessionId }))
  on('ui.log', () => ({ value: undefined }))
  on('http.fetch', (_$, e) => {
    const method = e.init?.method ?? 'GET'
    calls.push({ url: e.url, method, body: e.init?.body ? JSON.parse(e.init.body) : undefined, auth: e.init?.headers?.Authorization })
    if (e.url.endsWith('/hello')) return { value: { status: 200, ok: true, headers: {}, text: '{"address":"cplt-4a/comms-3a9e9c"}' } }
    if (e.url.endsWith('/peers')) return { value: { status: 200, ok: true, headers: {}, text: JSON.stringify({ peers: PEERS }) } }
    if (e.url.endsWith('/send')) return { value: { status: 200, ok: true, headers: {}, text: '{"id":"m_1","to":"shoggoth/art-0f7de4"}' } }
    if (e.url.endsWith('/bye')) return { value: { status: 204, ok: true, headers: {}, text: '' } }
    if (e.url.includes('/wait?')) {
      const next = waits.shift() ?? { status: 204, text: '' }
      return { value: { ...next, ok: next.status < 300, headers: {} } }
    }
    return { value: { status: 404, ok: false, headers: {}, text: '' } }
  })
  return calls
}

test('ListAgents keeps the local listing and adds only live peers on other machines', async ($, on) => {
  wire($, on, [])
  on('tool.call', () => ({ result: { listing: 'Local sessions (1):\n  hollowedoath-3e  ·  idle' } }))
  await $.session.start({ surface: null, isInteractive: false, cwd: 'C:/work/comms' })

  const out = await $.tool.call({ tool: 'ListAgents' })
  const listing = (out.result as { listing: string }).listing
  expect(listing).toContain('Local sessions (1)')
  expect(listing).toContain('agentbus:shoggoth/art-0f7de4')
  expect(listing).toContain('vm-shoggoth')
  expect(listing).not.toContain('other-111111')
  expect(listing).not.toContain('old-459b94')
  expect(listing).not.toContain('session-7ea579')
})

test('SendMessage to an agentbus name posts to the bus; other recipients pass through', async ($, on) => {
  const calls = wire($, on, [])
  on('tool.call', () => ({ result: { success: true, message: 'delivered locally' } }))
  await $.session.start({ surface: null, isInteractive: false, cwd: 'C:/work/comms' })

  const remote = await $.tool.call({ tool: 'SendMessage', to: 'agentbus:shoggoth/art-0f7de4', message: 'hi' })
  expect((remote.result as { success: boolean }).success).toBe(true)
  const send = calls.find(c => c.url.endsWith('/send'))
  expect(send?.url).toBe('http://bus.test:8317/v1/agentbus/send')
  expect(send?.body).toEqual({ from_session: '3a9e9c5c-0000-0000-0000-000000000000', to: 'shoggoth/art-0f7de4', body: 'hi' })
  expect(send?.auth).toBe('Bearer test-token')

  const local = await $.tool.call({ tool: 'SendMessage', to: 'hollowedoath-3e', message: 'hi' })
  expect((local.result as { message: string }).message).toBe('delivered locally')
})

test('an interactive session turns a bus message into a prompt with a reply address', async ($, on) => {
  const clock = mock.clock(on)
  const msg = { id: 'm_9', from: 'vm-shoggoth (shoggoth/art-0f7de4)', body: 'build is green', reply_to: 'm_1' }
  wire($, on, [{ status: 200, text: JSON.stringify({ messages: [msg] }) }])
  const prompts: string[] = []
  on('prompt.submit', (_$, e) => {
    prompts.push(e.text)
    return { text: e.text }
  })
  await $.session.start({ surface: 'terminal', isInteractive: true, cwd: 'C:/work/comms' })

  await clock.advance(1000)
  await clock.settle()
  expect(prompts.length).toBe(1)
  expect(prompts[0]).toContain('build is green')
  expect(prompts[0]).toContain('to: "agentbus:shoggoth/art-0f7de4"')
  expect(prompts[0]).toContain('in reply to m_1')
})

test('a non-interactive session never long-polls', async ($, on) => {
  const clock = mock.clock(on)
  const calls = wire($, on, [])
  await $.session.start({ surface: null, isInteractive: false, cwd: 'C:/work/comms' })
  await clock.advance(5000)
  expect(calls.some(c => c.url.includes('/wait?'))).toBe(false)
})

test('session.end with prompt_input_exit says bye and stops the wait loop', async ($, on) => {
  const clock = mock.clock(on)
  const calls = wire($, on, [])
  await $.session.start({ surface: 'terminal', isInteractive: true, cwd: 'C:/work/comms' })

  await $.session.end({ reason: 'prompt_input_exit', sessionId: DEFAULT_SESSION_ID, resume: { id: DEFAULT_SESSION_ID } })
  const bye = calls.find(c => c.url.endsWith('/bye'))
  expect(bye?.body).toEqual({ session: DEFAULT_SESSION_ID })

  calls.length = 0
  await clock.advance(5000)
  expect(calls.some(c => c.url.includes('/wait?'))).toBe(false)
})

test('session.end with clear says bye, then the next tick follows the session to its new id', async ($, on) => {
  const clock = mock.clock(on)
  const session = { id: DEFAULT_SESSION_ID }
  const calls = wire($, on, [], session)
  await $.session.start({ surface: 'terminal', isInteractive: true, cwd: 'C:/work/comms' })

  await $.session.end({ reason: 'clear', sessionId: session.id, resume: { id: session.id } })
  const bye = calls.find(c => c.url.endsWith('/bye'))
  expect(bye?.body).toEqual({ session: DEFAULT_SESSION_ID })

  // /clear doesn't fire session.start again; the next tick has to notice the new id itself.
  session.id = 'cleared1-0000-0000-0000-000000000000'
  calls.length = 0
  await clock.advance(1000)
  const hello = calls.find(c => c.url.endsWith('/hello'))
  expect(hello?.body).toMatchObject({ session: session.id, mod: true })
  const wait = calls.find(c => c.url.includes('/wait?'))
  expect(wait?.url).toContain(`session=${encodeURIComponent(session.id)}`)
})

test('without COMPUTERNAME the machine name comes from /etc/hostname', async ($, on) => {
  const calls: Call[] = []
  mock.env(on, { ANTHROPIC_BASE_URL: ENV.ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN: ENV.ANTHROPIC_AUTH_TOKEN })
  on('session.id', () => ({ value: DEFAULT_SESSION_ID }))
  on('session.start', () => ({ cwd: '/home/u/comms' }))
  on('ui.log', () => ({ value: undefined }))
  on('fs.read', () => ({ value: 'Fedora.localdomain\n' }))
  on('http.fetch', (_$, e) => {
    calls.push({ url: e.url, method: e.init?.method ?? 'GET', body: e.init?.body ? JSON.parse(e.init.body) : undefined, auth: undefined })
    return { value: { status: 200, ok: true, headers: {}, text: '{"address":"fedora/comms-3a9e9c"}' } }
  })
  await $.session.start({ surface: null, isInteractive: false, cwd: '/home/u/comms' })
  const hello = calls.find(c => c.url.endsWith('/hello'))
  expect((hello?.body as { machine: string }).machine).toBe('fedora')
})

test('a Slack message from an allowed user is framed as their instruction', async ($, on) => {
  const clock = mock.clock(on)
  const msg = { id: 'm_7', from: 'slack', body: 'please rebase', from_user: true, slack_user: 'jane' }
  wire($, on, [{ status: 200, text: JSON.stringify({ messages: [msg] }) }])
  const prompts: string[] = []
  on('prompt.submit', (_$, e) => {
    prompts.push(e.text)
    return { text: e.text }
  })
  await $.session.start({ surface: 'terminal', isInteractive: true, cwd: 'C:/work/comms' })

  await clock.advance(1000)
  await clock.settle()
  expect(prompts.length).toBe(1)
  expect(prompts[0]).toContain('from jane via Slack')
  expect(prompts[0]).toContain('relayed over the agentbus')
  expect(prompts[0]).toContain('instruction')
  expect(prompts[0]).toContain('please rebase')
  expect(prompts[0]).toContain('to: "agentbus:slack"')
  expect(prompts[0]).not.toContain('not from the user')
})
