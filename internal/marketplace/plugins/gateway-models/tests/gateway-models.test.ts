import { expect, test } from 'claude-code/testing'

const command = (name: string) => ({
  command: name, args: '', origin: { kind: 'composer' as const },
  presentation: { isFullscreen: false, columns: 80 },
})

test('setup runs the packaged installer with argv, not a shell string', async ($, on) => {
  const calls: string[][] = []
  on('process.run', (_$, e) => {
    calls.push([...e.argv])
    expect(e.init?.timeoutMs).toBe(15000)
    return { value: { exitCode: 0, stdout: 'Installed gateway-models.', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  const result = await $.command.run(command('gateway-models-setup'))
  expect(result.text).toBe('Installed gateway-models.')
  expect(result.exitCode).toBe(0)
  expect(calls.length).toBe(1)
  expect(calls[0]?.[0]).toBe('node')
  expect(calls[0]?.[1]?.endsWith('/hooks/sync.mjs')).toBe(true)
  expect(calls[0]?.[2]).toBe('--install')
})

test('manual sync never installs a launcher and preserves command errors', async ($, on) => {
  on('process.run', (_$, e) => {
    expect(e.argv.length).toBe(2)
    return { value: { exitCode: 1, stdout: '', stderr: 'gateway-models: settings.json is not valid JSON', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  const result = await $.command.run(command('gateway-models-sync'))
  expect(result.exitCode).toBe(1)
  expect(result.text).toContain('not valid JSON')
})
