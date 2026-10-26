import type { EngineInterface, Register } from 'claude-code'

async function synchronize($: EngineInterface, install: boolean) {
  return $.process.run(
    ['node', `${$.plugin.root}/hooks/sync.mjs`, ...(install ? ['--install'] : [])],
    { timeoutMs: 15000 },
  )
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'gateway-models-setup',
      description: 'Install gateway model syncing before Claude Code launches (once per client)',
    })
    await $.command.register({
      name: 'gateway-models-sync',
      description: 'Sync gateway model picker and compact settings for the next session',
    })
    return next(e)
  })
  on('command.run', { command: 'gateway-models-setup' }, async $ => {
    const result = await synchronize($, true)
    return { text: [result.stdout, result.stderr].filter(Boolean).join('\n') || 'Gateway model setup finished.', exitCode: result.exitCode }
  })
  on('command.run', { command: 'gateway-models-sync' }, async $ => {
    const result = await synchronize($, false)
    return { text: [result.stdout, result.stderr].filter(Boolean).join('\n') || 'Gateway model sync finished.', exitCode: result.exitCode }
  })
}
