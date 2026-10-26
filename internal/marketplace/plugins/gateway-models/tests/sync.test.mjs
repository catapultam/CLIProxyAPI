import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import http from 'node:http'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import {
  parseSnapshot, mergeSettings, sync, install, upsertBlock, effectiveCredentials, normalizeBase,
  parseSettingsText, SyncError, STATE_FILE, LOCK_FILE, BLOCK_START, BLOCK_END,
} from '../hooks/sync.mjs'

const SCRIPT = fileURLToPath(new URL('../hooks/sync.mjs', import.meta.url))
const TOKEN = 'sk-test-secret-token'
const ROW = { model: 'claude-gw-gpt-6.1-sol', label: 'GPT 6.1 Sol', behavesAs: 'claude-fable-5-1', autoCompactWindow: 500000, effortLevel: 'xhigh' }

const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), 'gm-sync-'))
const write = (dir, settings, text) => fs.writeFileSync(path.join(dir, 'settings.json'), text ?? JSON.stringify(settings, null, 2) + '\n')
const read = (dir) => JSON.parse(fs.readFileSync(path.join(dir, 'settings.json'), 'utf8').replace(/^\ufeff/, ''))
const fetchOf = (body, status = 200) => async () => new Response(typeof body === 'string' ? body : JSON.stringify(body), { status })
const dto = (models) => ({ version: 1, models })

function baseSettings(extra = {}) {
  return { env: { ANTHROPIC_BASE_URL: 'https://gw.example', ANTHROPIC_AUTH_TOKEN: TOKEN }, model: 'opus', ...extra }
}

async function withServer(handler, fn) {
  const server = http.createServer(handler)
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  try {
    await fn(`http://127.0.0.1:${server.address().port}`)
  } finally {
    await new Promise((resolve) => server.close(resolve))
  }
}

test('parseSnapshot accepts a valid DTO and whitelists fields', () => {
  const parsed = parseSnapshot({ version: 1, extra: 1, models: [{ ...ROW, hooks: 'x', env: { A: 'b' } }] })
  assert.deepEqual(parsed, { version: 1, models: [ROW] })
  assert.deepEqual(parseSnapshot({ version: 1, models: [] }), { version: 1, models: [] })
})

test('parseSnapshot rejects invalid lists as a whole', () => {
  const bad = (row) => parseSnapshot(dto([ROW, row]))
  assert.equal(parseSnapshot(null), undefined)
  assert.equal(parseSnapshot({ version: 2, models: [] }), undefined)
  assert.equal(parseSnapshot({ version: 1 }), undefined)
  assert.equal(bad({ model: 'gpt-x', behavesAs: 'claude-opus-5' }), undefined)
  assert.equal(bad({ model: 'claude-gw-a b', behavesAs: 'claude-opus-5' }), undefined)
  assert.equal(bad({ model: 'claude-gw-a', behavesAs: 'gpt-5' }), undefined)
  assert.equal(bad({ model: 'claude-gw-a', behavesAs: 'claude-opus-4.5' }), undefined)
  assert.equal(bad({ model: 'claude-gw-a', behavesAs: 'claude-opus-5', autoCompactWindow: 99999 }), undefined)
  assert.equal(bad({ model: 'claude-gw-a', behavesAs: 'claude-opus-5', autoCompactWindow: 1.5e5 + 0.5 }), undefined)
  assert.equal(bad({ model: 'claude-gw-a', behavesAs: 'claude-opus-5', effortLevel: 'max' }), undefined)
  assert.equal(bad({ model: 'claude-gw-a', behavesAs: 'claude-opus-5', label: 'x\ny' }), undefined)
  assert.equal(bad({ model: 'CLAUDE-GW-GPT-6.1-SOL', behavesAs: 'claude-opus-5' }), undefined, 'case-insensitive duplicate')
  assert.equal(parseSnapshot(dto(Array.from({ length: 201 }, (_, i) => ({ model: `claude-gw-m${i}`, behavesAs: 'claude-opus-5' })))), undefined)
})

test('mergeSettings preserves unrelated data and manual rows', () => {
  const settings = baseSettings({
    permissions: { allow: ['Bash(ls)'] },
    modelPicker: { replaceBuiltInOptions: true, options: [{ model: 'claude-manual', label: 'Manual', behavesAs: 'claude-opus-5' }, { model: 'claude-gw-old', behavesAs: 'claude-opus-5' }] },
    modelSettings: {
      'claude-gw-old': { autoCompactWindow: 200000, maxEffortLevel: 'high' },
      'claude-gw-gpt-6.1-sol': { maxEffortLevel: 'medium', effortLevel: 'low' },
      'claude-opus-5': { effortLevel: 'high' },
    },
  })
  const before = structuredClone(settings)
  const merged = mergeSettings(settings, parseSnapshot(dto([ROW])))
  assert.deepEqual(settings, before, 'input is not mutated')
  assert.equal(merged.model, 'opus')
  assert.deepEqual(merged.permissions, before.permissions)
  assert.equal(merged.modelPicker.replaceBuiltInOptions, true)
  assert.deepEqual(merged.modelPicker.options.map((r) => r.model), ['claude-manual', 'claude-gw-gpt-6.1-sol'])
  assert.deepEqual(merged.modelSettings['claude-gw-gpt-6.1-sol'], { maxEffortLevel: 'medium', effortLevel: 'xhigh', autoCompactWindow: 500000 })
  assert.deepEqual(merged.modelSettings['claude-gw-old'], { maxEffortLevel: 'high' }, 'only managed fields removed')
  assert.deepEqual(merged.modelSettings['claude-opus-5'], { effortLevel: 'high' })
})

test('mergeSettings uses canonical lowercase per-model keys', () => {
  const model = { ...ROW, model: ROW.model.toUpperCase() }
  const next = mergeSettings({ modelSettings: { [model.model]: { maxEffortLevel: 'high' } } }, parseSnapshot(dto([model])))
  assert.equal(next.modelSettings[model.model], undefined)
  assert.deepEqual(next.modelSettings[ROW.model], { maxEffortLevel: 'high', autoCompactWindow: 500000, effortLevel: 'xhigh' })
})

test('sync rejects a changed gateway connection before committing a stale snapshot', async () => {
  const dir = tmp()
  write(dir, baseSettings())
  await assert.rejects(sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])), beforeCommit: async attempt => {
    if (attempt === 0) write(dir, baseSettings({ env: { ANTHROPIC_BASE_URL: 'https://different.example', ANTHROPIC_AUTH_TOKEN: 'other-test' } }))
  } }), { code: 'connection-changed' })
  assert.equal(read(dir).env.ANTHROPIC_BASE_URL, 'https://different.example')
  assert.equal(read(dir).modelPicker, undefined)
})

test('mergeSettings empty snapshot on a clean file changes nothing', () => {
  const settings = baseSettings()
  assert.deepEqual(mergeSettings(settings, parseSnapshot(dto([]))), settings)
})

test('mergeSettings refuses unexpected shapes instead of clobbering', () => {
  assert.throws(() => mergeSettings({ modelPicker: 'x' }, parseSnapshot(dto([ROW]))), SyncError)
  assert.throws(() => mergeSettings({ modelPicker: { options: {} } }, parseSnapshot(dto([ROW]))), SyncError)
  assert.throws(() => mergeSettings({ modelSettings: [] }, parseSnapshot(dto([ROW]))), SyncError)
})

test('credentials: process env wins over settings.env', () => {
  const settings = { env: { ANTHROPIC_BASE_URL: 'https://a', ANTHROPIC_AUTH_TOKEN: 't1' } }
  assert.deepEqual(effectiveCredentials({}, settings), { base: 'https://a', token: 't1' })
  assert.deepEqual(effectiveCredentials({ ANTHROPIC_BASE_URL: 'https://b', ANTHROPIC_AUTH_TOKEN: 't2' }, settings), { base: 'https://b', token: 't2' })
  assert.equal(normalizeBase('https://u:p@host'), undefined)
  assert.equal(normalizeBase('ftp://host'), undefined)
  assert.equal(normalizeBase('https://Host:443/x/?q=1#f'), 'https://host/x')
})

test('sync writes rows, preserves BOM and CRLF, and does not churn', async () => {
  const dir = tmp()
  const text = '\ufeff' + JSON.stringify(baseSettings(), null, 4).replace(/\n/g, '\r\n') + '\r\n'
  write(dir, null, text)
  const first = await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) })
  assert.equal(first.status, 'updated')
  const raw = fs.readFileSync(path.join(dir, 'settings.json'), 'utf8')
  assert.ok(raw.startsWith('\ufeff{'), 'BOM kept')
  assert.ok(raw.includes('\r\n') && !/[^\r]\n/.test(raw), 'CRLF kept')
  assert.ok(raw.includes('\n    "env"'), 'indent kept')
  const settings = read(dir)
  assert.deepEqual(settings.modelPicker.options, [{ model: ROW.model, label: ROW.label, behavesAs: ROW.behavesAs }])
  assert.deepEqual(settings.modelSettings[ROW.model], { autoCompactWindow: 500000, effortLevel: 'xhigh' })
  const stat = fs.statSync(path.join(dir, 'settings.json'))
  const second = await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) })
  assert.equal(second.status, 'unchanged')
  assert.equal(fs.readFileSync(path.join(dir, 'settings.json'), 'utf8'), raw)
  assert.equal(fs.statSync(path.join(dir, 'settings.json')).mtimeMs, stat.mtimeMs)
  assert.ok(!fs.existsSync(path.join(dir, LOCK_FILE)), 'lock released')
  assert.deepEqual(fs.readdirSync(dir).filter((n) => n.endsWith('.tmp')), [])
})

test('sync rejects invalid or missing settings without touching them', async () => {
  const dir = tmp()
  await assert.rejects(sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) }), { code: 'settings-missing' })
  for (const text of ['{ not json', '', '[1]', '"x"']) {
    write(dir, null, text)
    await assert.rejects(sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) }), { code: 'settings-invalid' })
    assert.equal(fs.readFileSync(path.join(dir, 'settings.json'), 'utf8'), text)
  }
})

test('sync without credentials leaves settings alone and makes no request', async () => {
  const dir = tmp()
  write(dir, { model: 'opus' })
  let calls = 0
  const result = await sync({ configDir: dir, env: {}, fetch: async () => { calls++; return new Response('{}') } })
  assert.deepEqual([result.status, result.reason, calls], ['skipped', 'no-credentials', 0])
  write(dir, { env: { ANTHROPIC_BASE_URL: 'https://gw.example' } })
  assert.equal((await sync({ configDir: dir, env: {}, fetch: async () => { calls++; return new Response('{}') } })).reason, 'no-credentials')
  assert.equal(calls, 0)
})

test('sync uses process env over settings.env against a real HTTP fixture', async () => {
  await withServer((req, res) => {
    const ok = req.url === '/v1/claude-code/settings' && req.headers.authorization === 'Bearer env-token'
    res.writeHead(ok ? 200 : 401, { 'content-type': 'application/json' })
    res.end(JSON.stringify(ok ? dto([ROW]) : { error: 'no' }))
  }, async (base) => {
    const dir = tmp()
    write(dir, baseSettings({ env: { ANTHROPIC_BASE_URL: 'https://wrong.example', ANTHROPIC_AUTH_TOKEN: 'file-token' } }))
    const result = await sync({ configDir: dir, env: { ANTHROPIC_BASE_URL: base, ANTHROPIC_AUTH_TOKEN: 'env-token' } })
    assert.deepEqual([result.status, result.source], ['updated', 'remote'])
  })
})

test('redirects and non-200 responses count as failures', async () => {
  await withServer((req, res) => {
    if (req.url.startsWith('/v1/claude-code/settings')) { res.writeHead(302, { location: '/elsewhere' }); res.end() } else { res.writeHead(200); res.end('{}') }
  }, async (base) => {
    const dir = tmp()
    write(dir, baseSettings())
    const result = await sync({ configDir: dir, env: { ANTHROPIC_BASE_URL: base, ANTHROPIC_AUTH_TOKEN: TOKEN } })
    assert.deepEqual([result.status, result.reason], ['skipped', 'unavailable'])
  })
})

test('outage and bad responses use the same-gateway cache only', async () => {
  const dir = tmp()
  write(dir, baseSettings())
  assert.equal((await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) })).source, 'remote')
  const cache = fs.readFileSync(path.join(dir, STATE_FILE), 'utf8')
  assert.ok(!cache.includes(TOKEN), 'cache holds no credentials')

  // Reset picker rows, then recover them from the cache during an outage.
  write(dir, baseSettings())
  const outage = await sync({ configDir: dir, env: {}, fetch: async () => { throw new Error(`down ${TOKEN}`) } })
  assert.deepEqual([outage.status, outage.source], ['updated', 'cache'])
  assert.equal(read(dir).modelPicker.options.length, 1)

  // Invalid DTO is rejected as a whole and the cache stands.
  write(dir, baseSettings())
  const bad = await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW, { model: 'nope', behavesAs: 'claude-x' }])) })
  assert.equal(bad.source, 'cache')
  assert.equal(JSON.parse(fs.readFileSync(path.join(dir, STATE_FILE), 'utf8')).gateways['https://gw.example'].snapshot.models.length, 1)

  // A different gateway never sees that cache.
  const other = baseSettings({ env: { ANTHROPIC_BASE_URL: 'https://other.example', ANTHROPIC_AUTH_TOKEN: TOKEN } })
  write(dir, other)
  const result = await sync({ configDir: dir, env: {}, fetch: async () => { throw new Error('down') } })
  assert.deepEqual([result.status, result.reason], ['skipped', 'unavailable'])
  assert.deepEqual(read(dir), other)
})

test('a successful empty list is authoritative and removes managed data only', async () => {
  const dir = tmp()
  write(dir, baseSettings({
    modelPicker: { options: [{ model: 'claude-keep', behavesAs: 'claude-opus-5' }, { model: ROW.model, behavesAs: ROW.behavesAs }] },
    modelSettings: { [ROW.model]: { autoCompactWindow: 300000, effortLevel: 'low', maxEffortLevel: 'high' } },
  }))
  const result = await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([])) })
  assert.equal(result.status, 'updated')
  const settings = read(dir)
  assert.deepEqual(settings.modelPicker.options, [{ model: 'claude-keep', behavesAs: 'claude-opus-5' }])
  assert.deepEqual(settings.modelSettings, { [ROW.model]: { maxEffortLevel: 'high' } })
  assert.equal((await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([])) })).status, 'unchanged')
})

test('unknown top-level preferences and key order survive', async () => {
  const dir = tmp()
  const original = baseSettings({ zzz: { nested: [1, 2, { a: null }] }, theme: 'dark', hooks: { Stop: [] }, enabledPlugins: { 'x@y': true } })
  write(dir, original)
  await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) })
  const settings = read(dir)
  for (const key of Object.keys(original)) assert.deepEqual(settings[key], original[key])
  assert.deepEqual(Object.keys(settings).slice(0, Object.keys(original).length), Object.keys(original))
})

test('settings.json symlink is resolved and kept', async (t) => {
  const dir = tmp()
  const real = tmp()
  fs.writeFileSync(path.join(real, 'real-settings.json'), JSON.stringify(baseSettings(), null, 2) + '\n')
  try {
    fs.symlinkSync(path.join(real, 'real-settings.json'), path.join(dir, 'settings.json'))
  } catch {
    t.skip('symlinks are not permitted here')
    return
  }
  assert.equal((await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) })).status, 'updated')
  assert.ok(fs.lstatSync(path.join(dir, 'settings.json')).isSymbolicLink())
  assert.equal(JSON.parse(fs.readFileSync(path.join(real, 'real-settings.json'), 'utf8')).modelPicker.options.length, 1)
})

test('a held lock times out without changing anything, and a stale lock is reclaimed', async () => {
  const dir = tmp()
  write(dir, baseSettings())
  const before = fs.readFileSync(path.join(dir, 'settings.json'), 'utf8')
  fs.writeFileSync(path.join(dir, LOCK_FILE), '1\n')
  await assert.rejects(sync({ configDir: dir, env: {}, lockTimeoutMs: 120, fetch: fetchOf(dto([ROW])) }), { code: 'lock-timeout' })
  assert.equal(fs.readFileSync(path.join(dir, 'settings.json'), 'utf8'), before)
  assert.ok(fs.existsSync(path.join(dir, LOCK_FILE)), 'foreign lock is not removed')
  const old = new Date(Date.now() - 120000)
  fs.utimesSync(path.join(dir, LOCK_FILE), old, old)
  assert.equal((await sync({ configDir: dir, env: {}, lockTimeoutMs: 500, fetch: fetchOf(dto([ROW])) })).status, 'updated')
})

test('two concurrent syncs serialize and both succeed', async () => {
  const dir = tmp()
  write(dir, baseSettings())
  const results = await Promise.all([1, 2].map(() => sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) })))
  assert.deepEqual(results.map((r) => r.status).sort(), ['unchanged', 'updated'])
})

test('a concurrent external edit is merged on retry, not overwritten', async () => {
  const dir = tmp()
  write(dir, baseSettings())
  let edited = false
  const result = await sync({
    configDir: dir, env: {}, fetch: fetchOf(dto([ROW])),
    beforeCommit: () => {
      if (edited) return
      edited = true
      write(dir, baseSettings({ theme: 'light' }))
    },
  })
  assert.equal(result.status, 'updated')
  const settings = read(dir)
  assert.equal(settings.theme, 'light')
  assert.equal(settings.modelPicker.options.length, 1)
  assert.deepEqual(fs.readdirSync(dir).filter((n) => n.endsWith('.tmp')), [])
})

test('persistent concurrent edits give up after bounded retries', async () => {
  const dir = tmp()
  write(dir, baseSettings())
  let n = 0
  await assert.rejects(sync({
    configDir: dir, env: {}, fetch: fetchOf(dto([ROW])),
    beforeCommit: () => write(dir, baseSettings({ counter: ++n })),
  }), { code: 'concurrent-edit' })
  assert.equal(n, 3)
})

test('errors and output never contain credentials', async () => {
  const dir = tmp()
  write(dir, '', '{ bad ' + TOKEN)
  try {
    await sync({ configDir: dir, env: {}, fetch: fetchOf(dto([ROW])) })
    assert.fail('expected rejection')
  } catch (error) {
    assert.ok(!String(error.message).includes(TOKEN))
  }
  write(dir, baseSettings())
  const run = spawnSync(process.execPath, [SCRIPT, '--config-dir', dir], { encoding: 'utf8', env: { ...process.env, ANTHROPIC_BASE_URL: 'http://127.0.0.1:1', ANTHROPIC_AUTH_TOKEN: TOKEN } })
  assert.ok(!(run.stdout + run.stderr).includes(TOKEN))
  assert.equal(run.status, 0)
  const quiet = spawnSync(process.execPath, [SCRIPT, '--quiet', '--config-dir', path.join(dir, 'missing')], { encoding: 'utf8' })
  assert.equal(quiet.status, 1)
  assert.equal(quiet.stdout + quiet.stderr, '')
})

test('parseSettingsText accepts BOM and CRLF', () => {
  assert.deepEqual(parseSettingsText('\ufeff{\r\n  "a": 1\r\n}\r\n'), { a: 1 })
})

// ---------------------------------------------------------------------------
// install

test('upsertBlock is idempotent, single, and preserves other content', () => {
  const block = [BLOCK_START, 'line', BLOCK_END].join('\n')
  const once = upsertBlock('echo hi\n# keep', block)
  assert.equal(upsertBlock(once, block), once)
  const updated = upsertBlock(once, [BLOCK_START, 'new', BLOCK_END].join('\n'))
  assert.equal(updated.split(BLOCK_START).length - 1, 1)
  assert.ok(updated.includes('echo hi') && updated.includes('# keep') && updated.includes('new') && !updated.includes('line'))
  const doubled = once + '\n' + block + '\n'
  assert.equal(upsertBlock(doubled, block).split(BLOCK_START).length - 1, 1)
  const crlf = upsertBlock('a\r\nb\r\n', block)
  assert.ok(!/[^\r]\n/.test(crlf))
})

test('install generates launchers and one idempotent profile block with a backup', () => {
  const dir = tmp()
  const profile = path.join(tmp(), 'profile.ps1')
  fs.writeFileSync(profile, 'Set-Alias ll ls\r\n')
  const options = { configDir: dir, scriptPath: 'C:/plug ins/it\'s/sync.mjs', nodePath: 'C:/node/node.exe', profilePath: profile, profileKind: 'ps', platform: 'win32' }
  const first = install(options)
  assert.equal(first.profiles[0].changed, true)
  assert.equal(fs.readFileSync(`${profile}.gateway-models.bak`, 'utf8'), 'Set-Alias ll ls\r\n')
  const ps1 = fs.readFileSync(path.join(dir, 'gateway-models-launcher.ps1'), 'utf8')
  const sh = fs.readFileSync(path.join(dir, 'gateway-models-launcher.sh'), 'utf8')
  assert.ok(ps1.includes('Get-Command claude.exe -CommandType Application'))
  assert.ok(ps1.includes("'C:/plug ins/it''s/sync.mjs'") && ps1.includes('--quiet') && ps1.includes('ExpectingInput'))
  assert.ok(sh.includes('command claude "$@"') && sh.includes("'C:/plug ins/it'\\''s/sync.mjs'") && sh.includes('--quiet'))
  const content = fs.readFileSync(profile, 'utf8')
  assert.ok(content.startsWith('Set-Alias ll ls\r\n'))
  assert.equal(content.split(BLOCK_START).length - 1, 1)
  assert.ok(content.includes('gateway-models-launcher.ps1'))

  const second = install(options)
  assert.equal(second.profiles[0].changed, false)
  assert.equal(fs.readFileSync(profile, 'utf8'), content)

  const upgraded = install({ ...options, scriptPath: 'C:/plugins/v2/sync.mjs' })
  assert.equal(upgraded.profiles[0].changed, false, 'block points at the launcher, which is rewritten instead')
  assert.ok(fs.readFileSync(path.join(dir, 'gateway-models-launcher.ps1'), 'utf8').includes('C:/plugins/v2/sync.mjs'))
})

test('install picks the rc file from SHELL on POSIX and skips unknown shells', () => {
  const home = tmp()
  const dir = tmp()
  const result = install({ configDir: dir, homedir: home, platform: 'linux', env: { SHELL: '/bin/zsh' }, scriptPath: '/p/sync.mjs', nodePath: '/usr/bin/node' })
  assert.equal(result.profiles[0].file, path.join(home, '.zshrc'))
  assert.ok(fs.readFileSync(path.join(home, '.zshrc'), 'utf8').includes('gateway-models-launcher.sh'))
  assert.equal(fs.existsSync(path.join(home, '.zshrc.gateway-models.bak')), false, 'no backup for a new file')
  const bash = install({ configDir: dir, homedir: home, platform: 'linux', env: { SHELL: '/bin/bash' }, scriptPath: '/p/sync.mjs' })
  assert.equal(bash.profiles[0].file, path.join(home, '.bashrc'))
  const fish = install({ configDir: dir, homedir: home, platform: 'linux', env: { SHELL: '/usr/bin/fish' }, scriptPath: '/p/sync.mjs' })
  assert.equal(fish.profileSkipped, true)
})

test('the POSIX launcher runs the original claude once, with args, stdin and exit code', { skip: process.platform === 'win32' }, () => {
  const dir = tmp()
  const bin = tmp()
  fs.writeFileSync(path.join(bin, 'claude'), '#!/bin/sh\necho "args:$*"\ncat\nexit 7\n', { mode: 0o755 })
  install({ configDir: dir, scriptPath: SCRIPT, nodePath: process.execPath, profilePath: path.join(tmp(), 'rc'), platform: 'linux' })
  const run = spawnSync('sh', ['-c', `. '${path.join(dir, 'gateway-models-launcher.sh')}'; claude a "b c"`], {
    encoding: 'utf8', input: 'piped', env: { ...process.env, PATH: `${bin}${path.delimiter}${process.env.PATH}`, CLAUDE_CONFIG_DIR: dir },
  })
  assert.equal(run.status, 7)
  assert.equal(run.stdout, 'args:a b c\npiped')
})
