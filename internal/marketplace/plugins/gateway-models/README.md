# Gateway models

Edit gateway model labels, borrowed Claude behavior, reasoning effort and auto-compaction windows in CLIProxyAPI Management Center's **Gateway models** page. New entries are hidden from raw discovery by default so each model appears once.

## One-time client setup

Add the proxy's homelab marketplace if it is not already registered, then install:

```sh
claude plugin install gateway-models@homelab
```

In Claude Code, run `/gateway-models-setup` once. It synchronizes the settings and installs a small pre-launch integration in your shell profile. Start a new shell after installation. Thereafter, use `claude` normally: each launch fetches the panel's latest model configuration before Claude Code reads its settings. The Windows `claude2` launcher is supported through `CLAUDE_CONFIG_DIR`. Setup keeps a durable copy of the synchronizer outside the versioned plugin cache. Rerun `/gateway-models-setup` after updating the plugin to refresh that copy.

This integration needs Node.js. It uses the `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` already configured on the client and requires no management key. It never sends an inference or availability request. No credentials are stored in its cache or printed in diagnostics.

## What it changes

The server is authoritative for `claude-gw-*` picker rows and their effort/auto-compaction fields. The synchronizer merges only these into the user's settings file. It preserves the selected/default model, credentials, environment, permissions, plugins, native model settings, non-gateway picker rows, and other per-model fields. Removing a row centrally removes its managed client entries at the next launch. Empty lists are supported.

Malformed responses and outages do not wipe settings. A validated last-known-good snapshot for the same gateway is used when available. Writes are locked between synchronizers, checked for concurrent changes and replaced atomically; unchanged settings are not rewritten. Existing shell profiles are backed up before adding the marked integration block.

`/gateway-models-sync` can synchronize manually, but model-specific compact windows are read at Claude Code startup. Start a new session for those changes to take effect. A plugin hook alone cannot reliably update the compact window of an already running session, which is why the pre-launch integration is installed.
