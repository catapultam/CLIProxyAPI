// The action auto mode blocked and is now asking the user about.
export type Pending = { summary: string; secondsLeft: number }

// One row per open question, keyed by the denied call's tool_use_id, so
// parallel denials never wait on each other.
export type PendingRows = Record<string, Pending>

declare module 'claude-code' {
  interface PluginState {
    'denial-prompt': { pending: PendingRows }
  }
}
