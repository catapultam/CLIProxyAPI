declare module 'claude-code' {
  interface PluginState {
    'cache-compact': { lastRequestAt: number | null; isCompacted: boolean }
  }
}
