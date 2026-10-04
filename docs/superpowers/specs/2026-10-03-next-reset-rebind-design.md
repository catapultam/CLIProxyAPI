# next-reset: move bound sessions toward the account that resets first

Status: approved by Alex 2026-10-03. Extends the next-reset strategy ("closest to reset first", `040de017`).

## Problem

next-reset only decides cold bindings and failovers. Session affinity then keeps a session on its account for as long as it stays active. A long session bound to account B, which resets later, keeps spending B while account A, which resets sooner, lets quota expire unused. New sessions go to A, but long-lived ones never move.

## Policy (Alex's words: "always burn up accounts that are about to reset, the closer it is to resetting the more preferable moving it is")

On every request of a session that is bound to credential B, while the routing strategy is `next-reset` with session affinity on:

Let A be the credential next-reset would pick right now for this provider and model: Ready tier, not near full, earliest weekly reset. Move the session to A when ALL of these hold:

1. **A resets first.** A ≠ B, A's weekly reset is earlier than B's, and A is not near full. Near full means weekly ≥ 98%, or short (5h) window ≥ 90% while that window is still open; these are the existing next-reset constants.
2. **The move is affordable.** `cost_pct ≤ allowance_pct`, where:
   - `allowance_pct = A.weekly_remaining_pct / max(1, hours_until(A.weekly_reset))`. The closer A is to resetting, the larger the share of what it has left that a move may spend; in A's final hour, all of it.
   - `cost_pct` is the prompt-cache rewrite cost of moving this session, in percent of A's weekly quota (see below). A cold cache costs 0.
3. **Guards.** The move is skipped when any of these apply:
   - the request continues a message thread (a `thread` object with a previous message id);
   - the conversation contains advisor results (`advisor_redacted_result` or advisor tool-result blocks), which only the account that produced them can read;
   - the session was moved within the last hour, which prevents flapping;
   - A's 5h window is near full.

A move re-binds the session's affinity entry to A, so later requests stay on A by ordinary affinity. It logs one Info line:

`next-reset: moved session | session=<truncated> from=<hash> to=<hash> cost=<pct> allowance=<pct> a_reset=<RFC3339> b_reset=<RFC3339> cold=<bool>`

Credentials are identified by the same opaque SHA-256 prefix the cold-pick log uses. Never log an ID, file name, label or email.

Moves only ever go toward the account that resets sooner. When that account becomes near full, the near-full rule makes the next one the target.

## Cost of a move

- **Context size:** the session's last observed request size in tokens: `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` from the last response usage seen for this session. If the proxy hasn't seen a response for this session yet, the cost is unknown and the session doesn't move. A cold binding already uses next-reset.
- **Cold cache: cost 0.** The cache is cold when either:
  - the time since the session's last request exceeds its cache lifetime: 1h if the request's `cache_control` blocks use `ttl: "1h"`, else 5 minutes; or
  - the request is the first after a compaction, as the proxy already detects it (`IsCompactionMetadataKey`, the `x-claude-code-context-compacted` header when present).
- **Warm cache:** `cost_tokens = context_tokens × (write_mult − read_mult)`, where `write_mult` is 2.0 for a 1h TTL and 1.25 for 5 minutes, and `read_mult` is 0.1. These are the API's cache price multipliers relative to input.
- **Conversion to percent:** `cost_pct = cost_tokens / A.tokens_per_pct`, with `tokens_per_pct` learned per credential (next section). While a credential has no estimate, warm-cache moves to it are not allowed; cold-cache moves are.

## Learning tokens per 1% of weekly quota

Per credential, from responses the proxy already handles:
- Track the weighted tokens spent since the last observed change of `anthropic-ratelimit-unified-7d-utilization`. Weighted tokens are `input + 1.25×cache_creation_5m + 2×cache_creation_1h + 0.1×cache_read + 5×output`; when the TTL split is unknown, count cache_creation at 1.25.
- When the utilization header increases by Δ (a fraction, e.g. 0.01), one sample is `accumulated / (Δ × 100)`. Reset the accumulator.
- Keep an EWMA (α = 0.3) of the samples. The estimate is valid after 3 samples.
- Decreases (a reset) clear the accumulator but keep the estimate.
- Keep it in memory; persisting it alongside next-reset state is optional.

## Out of scope

- Moves toward a later-resetting account.
- Codex-specific cache economics: Codex credentials use the same rule, but with no cache data they only move when the cost is 0.
- Any change to cold bindings, failover or exhaustion handling.
- An auto-reset quota option. Alex has parked it.

## Testing

Use deterministic clocks; no sleeps. Cover:
- no move when A resets later, when A is near full on weekly or on an open short window, or within an hour of the last move;
- no move when the request has a thread or advisor results;
- a cold cache from idle time (both TTLs) and from compaction moves regardless of cost;
- with a warm cache, the boundary `cost == allowance` moves, `cost > allowance` stays, and the allowance grows as A's reset nears (48h out vs 3h out vs the last hour);
- an unknown tokens_per_pct blocks warm moves and allows cold ones;
- the learning estimate becomes valid after 3 samples, follows the EWMA, and a utilization decrease resets the accumulator;
- after a move, later requests stay on A by affinity;
- the log line carries hashes only: the test credential has an email-bearing ID and label, and neither appears.
