// The action auto mode blocked and is now asking the user about.
export type Pending = { summary: string; secondsLeft: number }

declare module 'claude-code' {
  interface PluginState {
    'denial-prompt': { pending: Pending | null }
  }
}
