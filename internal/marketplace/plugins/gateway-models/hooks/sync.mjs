#!/usr/bin/env node
// Synchronizes gateway model picker rows and per-model settings from the
// gateway's /v1/claude-code/settings endpoint into the user's settings.json
// before Claude Code starts. No dependencies; Node 22+.
//
//   node sync.mjs [--config-dir path] [--quiet] [--install]
//
// Owned data: modelPicker.options rows and modelSettings entries whose model id
// starts with "claude-gw-", limited to the fields autoCompactWindow and
// effortLevel. Everything else in the file is left as it was.

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import crypto from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { execFileSync } from 'node:child_process'

export const PREFIX = 'claude-gw-'
export const STATE_FILE = 'gateway-models-state.json'
export const LOCK_FILE = 'gateway-models.lock'
export const MANAGED_FIELDS = ['autoCompactWindow', 'effortLevel']
export const BLOCK_START = '# >>> gateway-models launcher >>>'
export const BLOCK_END = '# <<< gateway-models launcher <<<'
const EFFORTS = ['low', 'medium', 'high', 'xhigh']
const MAX_BODY_BYTES = 1024 * 1024
const MAX_ATTEMPTS = 3
const STALE_LOCK_MS = 30000

const record = (value) => typeof value === 'object' && value !== null && !Array.isArray(value)
const isGateway = (id) => typeof id === 'string' && id.toLowerCase().startsWith(PREFIX)
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

export class SyncError extends Error {
  constructor(code, message) {
    super(message)
    this.name = 'SyncError'
    this.code = code
  }
}

// ---------------------------------------------------------------------------
// Snapshot validation

// Builds fresh, whitelisted objects. Any invalid row rejects the whole list.
export function parseSnapshot(value) {
  if (!record(value) || value.version !== 1 || !Array.isArray(value.models) || value.models.length > 200) return undefined
  const models = []
  const seen = new Set()
  for (const raw of value.models) {
    if (!record(raw) || typeof raw.model !== 'string' ||
        !/^claude-gw-[a-z0-9][a-z0-9._:/-]*$/i.test(raw.model) || raw.model.length > 266 ||
        typeof raw.behavesAs !== 'string' || !/^claude-[a-z0-9-]+$/i.test(raw.behavesAs) || raw.behavesAs.length > 256) return undefined
    const key = raw.model.toLowerCase()
    if (seen.has(key)) return undefined
    seen.add(key)
    const row = { model: raw.model, behavesAs: raw.behavesAs }
    if (raw.label !== undefined) {
      if (typeof raw.label !== 'string' || Buffer.byteLength(raw.label, 'utf8') > 256 || /[\x00-\x1f\x7f-\x9f]/.test(raw.label)) return undefined
      row.label = raw.label
    }
    if (raw.autoCompactWindow !== undefined) {
      if (!Number.isInteger(raw.autoCompactWindow) || raw.autoCompactWindow < 100000 || raw.autoCompactWindow > 1000000) return undefined
      row.autoCompactWindow = raw.autoCompactWindow
    }
    if (raw.effortLevel !== undefined) {
      if (typeof raw.effortLevel !== 'string' || !EFFORTS.includes(raw.effortLevel)) return undefined
      row.effortLevel = raw.effortLevel
    }
    models.push(row)
  }
  return { version: 1, models }
}

// ---------------------------------------------------------------------------
// Settings text handling

export function parseSettingsText(text) {
  const body = text.charCodeAt(0) === 0xfeff ? text.slice(1) : text
  let value
  try {
    value = JSON.parse(body)
  } catch {
    throw new SyncError('settings-invalid', 'settings.json is not valid JSON')
  }
  if (!record(value)) throw new SyncError('settings-invalid', 'settings.json must contain a JSON object')
  return value
}

export function detectFormat(text) {
  const bom = text.charCodeAt(0) === 0xfeff
  const body = bom ? text.slice(1) : text
  const crlf = (body.match(/\r\n/g) || []).length
  const lf = (body.match(/\n/g) || []).length - crlf
  const indentMatch = /^([ \t]+)"/m.exec(body)
  let indent = 2
  if (indentMatch) indent = indentMatch[1][0] === '\t' ? '\t' : Math.min(indentMatch[1].length, 8)
  return { bom, eol: crlf > lf ? '\r\n' : '\n', indent, trailingNewline: /\n$/.test(body) }
}

export function serializeSettings(value, format) {
  let out = JSON.stringify(value, null, format.indent)
  if (format.eol === '\r\n') out = out.replace(/\n/g, '\r\n')
  if (format.trailingNewline) out += format.eol
  return (format.bom ? '﻿' : '') + out
}

// ---------------------------------------------------------------------------
// Merge

// Returns a new settings object. The input is not modified. Throws SyncError
// when a managed container has an unexpected shape, so nothing is clobbered.
export function mergeSettings(settings, snapshot) {
  const next = structuredClone(settings)
  const wanted = new Map(snapshot.models.map((m) => [m.model.toLowerCase(), m]))

  // Picker rows.
  const hasGatewayRows = record(next.modelPicker) && Array.isArray(next.modelPicker.options) &&
    next.modelPicker.options.some((row) => record(row) && isGateway(row.model))
  if (wanted.size > 0 || hasGatewayRows) {
    if (next.modelPicker !== undefined && !record(next.modelPicker)) {
      throw new SyncError('settings-shape', 'modelPicker has an unexpected shape')
    }
    const picker = next.modelPicker ?? {}
    if (picker.options !== undefined && !Array.isArray(picker.options)) {
      throw new SyncError('settings-shape', 'modelPicker.options has an unexpected shape')
    }
    const emitted = new Set()
    const options = []
    for (const row of picker.options ?? []) {
      if (record(row) && isGateway(row.model)) {
        const key = row.model.toLowerCase()
        const model = wanted.get(key)
        if (model && !emitted.has(key)) {
          emitted.add(key)
          options.push(pickerRow(model))
        }
        continue
      }
      options.push(row)
    }
    for (const model of snapshot.models) {
      if (!emitted.has(model.model.toLowerCase())) options.push(pickerRow(model))
    }
    picker.options = options
    next.modelPicker = picker
  }

  // Per-model settings: only the managed fields of gateway entries.
  const hasGatewaySettings = record(next.modelSettings) && Object.keys(next.modelSettings).some(isGateway)
  const wantsFields = snapshot.models.some((m) => MANAGED_FIELDS.some((f) => m[f] !== undefined))
  if (wantsFields || hasGatewaySettings) {
    if (next.modelSettings !== undefined && !record(next.modelSettings)) {
      throw new SyncError('settings-shape', 'modelSettings has an unexpected shape')
    }
    const all = next.modelSettings ?? {}
    const existing = new Map()
    for (const key of Object.keys(all)) if (isGateway(key)) existing.set(key.toLowerCase(), key)
    const touched = new Set()
    for (const model of snapshot.models) {
      const lower = model.model.toLowerCase()
      const previousKey = existing.get(lower)
      // Claude Code canonicalizes unknown model IDs to lowercase for modelSettings.
      const key = model.model.toLowerCase()
      touched.add(lower)
      const hasFields = MANAGED_FIELDS.some((f) => model[f] !== undefined)
      let entry = all[previousKey ?? key]
      if (entry !== undefined && !record(entry)) {
        if (!hasFields) continue
        throw new SyncError('settings-shape', 'a modelSettings entry has an unexpected shape')
      }
      if (entry === undefined) {
        if (!hasFields) continue
        entry = {}
      }
      for (const field of MANAGED_FIELDS) {
        if (model[field] !== undefined) entry[field] = model[field]
        else delete entry[field]
      }
      if (previousKey && previousKey !== key) delete all[previousKey]
      if (Object.keys(entry).length === 0) delete all[key]
      else all[key] = entry
    }
    for (const [lower, key] of existing) {
      if (touched.has(lower)) continue
      const entry = all[key]
      if (!record(entry)) continue
      for (const field of MANAGED_FIELDS) delete entry[field]
      if (Object.keys(entry).length === 0) delete all[key]
    }
    if (next.modelSettings !== undefined || Object.keys(all).length > 0) next.modelSettings = all
  }
  return next
}

function pickerRow(model) {
  return { model: model.model, ...(model.label ? { label: model.label } : {}), behavesAs: model.behavesAs }
}

// ---------------------------------------------------------------------------
// Credentials and fetching

export function resolveConfigDir(argvDir, env = process.env, home = os.homedir()) {
  if (argvDir) return path.resolve(argvDir)
  if (env.CLAUDE_CONFIG_DIR) return path.resolve(env.CLAUDE_CONFIG_DIR)
  return path.join(home, '.claude')
}

// Process environment wins over the parsed user settings env, because this
// runs before Claude Code applies settings.env.
export function effectiveCredentials(env, settings) {
  const fromSettings = record(settings.env) ? settings.env : {}
  const text = (value) => typeof value === 'string' && value.trim() !== '' ? value.trim() : undefined
  const envBase = text(env.ANTHROPIC_BASE_URL)
  const envToken = text(env.ANTHROPIC_AUTH_TOKEN)
  // Never send a settings-file credential to an unrelated process-env endpoint.
  // A partial override is intentionally incomplete and causes sync to skip.
  if (envBase || envToken) return { base: envBase, token: envToken }
  return { base: text(fromSettings.ANTHROPIC_BASE_URL), token: text(fromSettings.ANTHROPIC_AUTH_TOKEN) }
}

// Cache key and request origin: scheme, host, port and path, no credentials,
// query, fragment or trailing slash. Returns undefined for unusable values.
export function normalizeBase(raw) {
  let url
  try {
    url = new URL(raw)
  } catch {
    return undefined
  }
  if ((url.protocol !== 'https:' && url.protocol !== 'http:') || url.username || url.password) return undefined
  return `${url.origin}${url.pathname.replace(/\/+$/, '')}`
}

export async function fetchSnapshot(base, token, fetchImpl = globalThis.fetch, timeoutMs = 3000) {
  try {
    const response = await fetchImpl(`${base}/v1/claude-code/settings`, {
      method: 'GET',
      redirect: 'error',
      headers: { Authorization: `Bearer ${token}`, Accept: 'application/json' },
      signal: AbortSignal.timeout(timeoutMs),
    })
    if (!response.ok) return undefined
    const text = await response.text()
    if (text.length > MAX_BODY_BYTES) return undefined
    return parseSnapshot(JSON.parse(text))
  } catch {
    return undefined
  }
}

// ---------------------------------------------------------------------------
// Files

export function atomicWrite(file, data, mode) {
  const tmp = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.${crypto.randomBytes(4).toString('hex')}.tmp`)
  try {
    fs.writeFileSync(tmp, data, { flag: 'wx', mode })
    fs.renameSync(tmp, file)
  } catch (error) {
    try { fs.unlinkSync(tmp) } catch { /* already gone */ }
    throw error
  }
}

async function acquireLock(configDir, timeoutMs) {
  const lock = path.join(configDir, LOCK_FILE)
  const deadline = Date.now() + timeoutMs
  for (;;) {
    try {
      const fd = fs.openSync(lock, 'wx', 0o600)
      fs.writeSync(fd, `${process.pid}\n`)
      fs.closeSync(fd)
      return () => { try { fs.unlinkSync(lock) } catch { /* already gone */ } }
    } catch (error) {
      if (error.code !== 'EEXIST') throw new SyncError('lock-failed', 'could not create the lock file')
    }
    try {
      if (Date.now() - fs.statSync(lock).mtimeMs > STALE_LOCK_MS) {
        fs.unlinkSync(lock)
        continue
      }
    } catch { /* lock vanished; retry */ }
    if (Date.now() >= deadline) throw new SyncError('lock-timeout', 'another sync holds the lock')
    await sleep(25)
  }
}

function readCache(configDir, base) {
  try {
    const parsed = JSON.parse(fs.readFileSync(path.join(configDir, STATE_FILE), 'utf8'))
    const entry = record(parsed) && record(parsed.gateways) ? parsed.gateways[base] : undefined
    return record(entry) ? parseSnapshot(entry.snapshot) : undefined
  } catch {
    return undefined
  }
}

function writeCache(configDir, base, snapshot) {
  const file = path.join(configDir, STATE_FILE)
  try {
    let current = {}
    try {
      const parsed = JSON.parse(fs.readFileSync(file, 'utf8'))
      if (record(parsed) && record(parsed.gateways)) current = parsed.gateways
    } catch { /* start fresh */ }
    if (JSON.stringify(parseSnapshot(current[base]?.snapshot)) === JSON.stringify(snapshot)) return
    const gateways = { ...current, [base]: { snapshot, updatedAt: new Date().toISOString() } }
    atomicWrite(file, JSON.stringify({ version: 1, gateways }, null, 2) + '\n', 0o600)
  } catch { /* cache is best effort */ }
}

// ---------------------------------------------------------------------------
// sync()

// Result: { status: 'updated' | 'unchanged' | 'skipped', source: 'remote' | 'cache' | 'none', reason? }
// Throws SyncError when settings.json is missing, invalid or cannot be written.
export async function sync(options = {}) {
  const env = options.env ?? process.env
  const configDir = resolveConfigDir(options.configDir, env, options.homedir)
  const settingsLink = path.join(configDir, 'settings.json')
  let settingsPath
  try {
    settingsPath = fs.realpathSync(settingsLink)
  } catch {
    throw new SyncError('settings-missing', 'settings.json was not found')
  }

  const release = await acquireLock(configDir, options.lockTimeoutMs ?? 5000)
  try {
    let bytes = fs.readFileSync(settingsPath)
    const settings = parseSettingsText(bytes.toString('utf8'))
    const { base: rawBase, token } = effectiveCredentials(env, settings)
    const base = rawBase ? normalizeBase(rawBase) : undefined
    if (!base || !token) return { status: 'skipped', source: 'none', reason: 'no-credentials' }

    let snapshot = await fetchSnapshot(base, token, options.fetch ?? globalThis.fetch, options.timeoutMs ?? 3000)
    let source = 'remote'
    if (snapshot) {
      writeCache(configDir, base, snapshot)
    } else {
      snapshot = readCache(configDir, base)
      source = 'cache'
    }
    if (!snapshot) return { status: 'skipped', source: 'none', reason: 'unavailable' }

    const mode = fs.statSync(settingsPath).mode & 0o777
    for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
      if (attempt > 0) bytes = fs.readFileSync(settingsPath)
      const text = bytes.toString('utf8')
      const current = parseSettingsText(text)
      const connection = effectiveCredentials(env, current)
      if (normalizeBase(connection.base) !== base || connection.token !== token) {
        throw new SyncError('connection-changed', 'gateway connection changed during the sync')
      }
      const merged = mergeSettings(current, snapshot)
      if (JSON.stringify(merged) === JSON.stringify(current)) return { status: 'unchanged', source }
      const out = Buffer.from(serializeSettings(merged, detectFormat(text)), 'utf8')
      const tmp = path.join(path.dirname(settingsPath), `.${path.basename(settingsPath)}.${process.pid}.${crypto.randomBytes(4).toString('hex')}.tmp`)
      fs.writeFileSync(tmp, out, { flag: 'wx', mode })
      try {
        if (options.beforeCommit) await options.beforeCommit(attempt)
        if (!fs.readFileSync(settingsPath).equals(bytes)) {
          fs.unlinkSync(tmp)
          continue
        }
        fs.renameSync(tmp, settingsPath)
        return { status: 'updated', source }
      } catch (error) {
        try { fs.unlinkSync(tmp) } catch { /* renamed or gone */ }
        throw error instanceof SyncError ? error : new SyncError('write-failed', 'could not write settings.json')
      }
    }
    throw new SyncError('concurrent-edit', 'settings.json kept changing during the sync')
  } finally {
    release()
  }
}

// ---------------------------------------------------------------------------
// Launcher generation and install

const psQuote = (value) => `'${String(value).replace(/'/g, "''")}'`
const shQuote = (value) => `'${String(value).replace(/'/g, `'\\''`)}'`

export function generatePowerShellLauncher(scriptPath, nodePath) {
  return [
    '# Generated by gateway-models --install. Do not edit; rerun --install to regenerate.',
    'function claude {',
    `    $syncScript = ${psQuote(scriptPath)}`,
    `    $fallbackNode = ${psQuote(nodePath || '')}`,
    '    $original = Get-Command claude.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1',
    '    if (-not $original) {',
    '        Write-Error "claude.exe was not found on PATH"',
    '        return',
    '    }',
    '    $node = Get-Command node -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1',
    '    $nodePath = if ($node) { $node.Source } elseif ($fallbackNode -and (Test-Path -LiteralPath $fallbackNode)) { $fallbackNode } else { $null }',
    '    if ($nodePath -and (Test-Path -LiteralPath $syncScript)) {',
    '        try { & $nodePath $syncScript --quiet *> $null } catch { }',
    '    }',
    '    if ($MyInvocation.ExpectingInput) { $input | & $original.Source @args } else { & $original.Source @args }',
    '}',
    '',
  ].join('\n')
}

export function generatePosixLauncher(scriptPath, nodePath) {
  return [
    '# Generated by gateway-models --install. Do not edit; rerun --install to regenerate.',
    'claude() {',
    `    __gm_script=${shQuote(scriptPath)}`,
    `    __gm_node=$(command -v node 2>/dev/null || true)`,
    `    if [ -z "$__gm_node" ] && [ -x ${shQuote(nodePath || '')} ]; then __gm_node=${shQuote(nodePath || '')}; fi`,
    '    if [ -n "$__gm_node" ] && [ -f "$__gm_script" ]; then',
    '        "$__gm_node" "$__gm_script" --quiet </dev/null >/dev/null 2>&1 || true',
    '    fi',
    '    unset __gm_script __gm_node',
    '    command claude "$@"',
    '}',
    '',
  ].join('\n')
}

// Replaces the first marked block in place, removes any others, or appends one.
export function upsertBlock(text, block) {
  const eol = (text.match(/\r\n/g) || []).length > (text.match(/\n/g) || []).length / 2 ? '\r\n' : '\n'
  const body = block.split('\n').join(eol)
  const pattern = new RegExp(`[ \\t]*${escapeRegExp(BLOCK_START)}[\\s\\S]*?${escapeRegExp(BLOCK_END)}[ \\t]*(\\r?\\n)?`, 'g')
  let replaced = false
  let out = text.replace(pattern, () => {
    if (replaced) return ''
    replaced = true
    return body + eol
  })
  if (!replaced) {
    const lead = out === '' || /\n$/.test(out) ? '' : eol
    out = out + lead + body + eol
  }
  return out
}

const escapeRegExp = (value) => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

function profileBlock(kind, launcherPath) {
  if (kind === 'ps') {
    return [BLOCK_START, `if (Test-Path -LiteralPath ${psQuote(launcherPath)}) { . ${psQuote(launcherPath)} }`, BLOCK_END].join('\n')
  }
  return [BLOCK_START, `[ -f ${shQuote(launcherPath)} ] && . ${shQuote(launcherPath)}`, BLOCK_END].join('\n')
}

function updateProfile(file, kind, launcherPath) {
  fs.mkdirSync(path.dirname(file), { recursive: true })
  let existing = ''
  let mode = 0o644
  let exists = false
  try {
    existing = fs.readFileSync(file, 'utf8')
    mode = fs.statSync(file).mode & 0o777
    exists = true
  } catch (error) {
    if (error.code !== 'ENOENT') throw new SyncError('profile-failed', 'could not read a shell profile')
  }
  const next = upsertBlock(existing, profileBlock(kind, launcherPath))
  if (next === existing) return { file, changed: false }
  let backup
  if (exists && existing.trim() !== '' && !existing.includes(BLOCK_START)) {
    backup = `${file}.gateway-models.bak`
    if (!fs.existsSync(backup)) fs.copyFileSync(file, backup)
  }
  atomicWrite(file, next, mode)
  return { file, changed: true, backup }
}

function discoverPowerShellProfile(env, home, exec) {
  try {
    const out = exec('pwsh', ['-NoProfile', '-Command', '$PROFILE'], { encoding: 'utf8', timeout: 8000, stdio: ['ignore', 'pipe', 'ignore'] }).trim()
    if (out) return out
  } catch { /* fall through to the conventional location */ }
  return path.join(home, 'Documents', 'PowerShell', 'Microsoft.PowerShell_profile.ps1')
}

function discoverPosixProfile(env, home) {
  const shell = path.basename(env.SHELL || '')
  if (shell === 'zsh') return path.join(env.ZDOTDIR || home, '.zshrc')
  if (shell === 'bash') return path.join(home, '.bashrc')
  return undefined
}

// Writes both launchers and one marked block in the current shell's profile.
// `profilePath` overrides discovery (used by tests and by callers that know it).
export function install(options = {}) {
  const env = options.env ?? process.env
  const home = options.homedir ?? os.homedir()
  const platform = options.platform ?? process.platform
  const configDir = resolveConfigDir(options.configDir, env, home)
  const sourcePath = options.scriptPath ?? fileURLToPath(import.meta.url)
  const nodePath = options.nodePath ?? process.execPath
  fs.mkdirSync(configDir, { recursive: true })
  // Installed plugin caches are versioned and can be cleaned up. Launch from
  // a durable client-owned copy, refreshed whenever setup runs after an update.
  const scriptPath = path.join(configDir, 'gateway-models-sync.mjs')
  const source = fs.readFileSync(sourcePath)
  let current
  try { current = fs.readFileSync(scriptPath) } catch { /* not installed yet */ }
  if (!current?.equals(source)) atomicWrite(scriptPath, source, 0o644)

  const ps1 = path.join(configDir, 'gateway-models-launcher.ps1')
  const sh = path.join(configDir, 'gateway-models-launcher.sh')
  atomicWrite(ps1, generatePowerShellLauncher(scriptPath, nodePath), 0o644)
  atomicWrite(sh, generatePosixLauncher(scriptPath, nodePath), 0o644)

  const profiles = []
  if (options.profilePath) {
    profiles.push({ file: options.profilePath, kind: options.profileKind ?? (platform === 'win32' ? 'ps' : 'sh') })
  } else if (platform === 'win32') {
    profiles.push({ file: discoverPowerShellProfile(env, home, options.execFile ?? execFileSync), kind: 'ps' })
  } else {
    const rc = discoverPosixProfile(env, home)
    if (rc) profiles.push({ file: rc, kind: 'sh' })
  }
  const updated = profiles.map(({ file, kind }) => updateProfile(file, kind, kind === 'ps' ? ps1 : sh))
  return { launchers: [ps1, sh], profiles: updated, profileSkipped: profiles.length === 0 }
}

// ---------------------------------------------------------------------------
// CLI

export function parseArgs(argv) {
  const args = { quiet: false, install: false, configDir: undefined }
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--quiet') args.quiet = true
    else if (argv[i] === '--install') args.install = true
    else if (argv[i] === '--config-dir') args.configDir = argv[++i]
    else if (argv[i].startsWith('--config-dir=')) args.configDir = argv[i].slice('--config-dir='.length)
  }
  return args
}

export async function main(argv = process.argv.slice(2)) {
  const args = parseArgs(argv)
  const say = (message) => { if (!args.quiet) process.stdout.write(`${message}\n`) }
  try {
    if (args.install) {
      const result = install({ configDir: args.configDir })
      say(`Launchers: ${result.launchers.join(', ')}`)
      for (const profile of result.profiles) say(`${profile.changed ? 'Updated' : 'Unchanged'} profile: ${profile.file}`)
      if (result.profileSkipped) say('No profile was updated for this shell; source the .sh launcher manually.')
      let initial
      try {
        const synced = await sync({ configDir: args.configDir })
        initial = `initial sync ${synced.status} (${synced.source}${synced.reason ? `, ${synced.reason}` : ''})`
      } catch (error) {
        initial = `initial sync failed: ${error instanceof SyncError ? error.message : 'unexpected failure'}`
      }
      say(`Installed gateway-models; ${initial}.`)
      say('Restart your shell once so the launcher loads. Later launches fetch gateway models before Claude starts.')
      return 0
    }
    const result = await sync({ configDir: args.configDir })
    say(`gateway-models: ${result.status} (${result.source}${result.reason ? `, ${result.reason}` : ''})`)
    return 0
  } catch (error) {
    // Messages are fixed strings and never include settings content or credentials.
    if (!args.quiet) process.stderr.write(`gateway-models: ${error instanceof SyncError ? error.message : 'unexpected failure'}\n`)
    return 1
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await main()
}
