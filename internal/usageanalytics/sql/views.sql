-- Subscription analytics: dashboard views. Apply as admin after schema.sql.
-- Re-running this file replaces the views (DROP ... CASCADE, then CREATE).
--
-- Core idea: between two reported usage percentages of the same account and
-- the same quota window, count the proxy traffic that ended in between.
-- Pooled over a continuous span, sum(traffic) / sum(delta percent) is the
-- DEMONSTRATED traffic per percentage point. It is an observation, not a
-- provider limit: traffic outside this proxy, header lag and in-flight
-- requests all move it. Every interval carries flags for these cases.

DROP VIEW IF EXISTS subscription_analytics.capacity_daily CASCADE;
DROP VIEW IF EXISTS subscription_analytics.capacity_span_model_mix CASCADE;
DROP VIEW IF EXISTS subscription_analytics.capacity_spans CASCADE;
DROP VIEW IF EXISTS subscription_analytics.quota_intervals CASCADE;
DROP VIEW IF EXISTS subscription_analytics.collector_gaps CASCADE;
DROP VIEW IF EXISTS subscription_analytics.collector_health CASCADE;
DROP VIEW IF EXISTS subscription_analytics.usage_hourly CASCADE;
DROP VIEW IF EXISTS subscription_analytics.usage_daily CASCADE;
DROP VIEW IF EXISTS subscription_analytics.error_categories_daily CASCADE;

-- Collector runs: one row per run_id.
CREATE VIEW subscription_analytics.collector_health AS
SELECT run_id,
       min(at)                                   AS started_at,
       max(at)                                   AS last_heartbeat_at,
       bool_or(kind = 'stop')                    AS clean_stop,   -- false: crash, kill, or close deadline
       max(queue_dropped)                        AS queue_dropped,
       max(spool_dropped)                        AS spool_dropped,
       max(spooled)                              AS spooled,
       max(spool_bytes) FILTER (WHERE at = (SELECT max(h2.at) FROM subscription_analytics.collector_heartbeats h2
                                            WHERE h2.run_id = h.run_id)) AS last_spool_bytes,
       max(write_errors)                         AS write_errors
FROM subscription_analytics.collector_heartbeats h
GROUP BY run_id;

-- Periods with no collector heartbeat for more than 3 minutes (heartbeat is 1 minute).
-- A gap means records in that period can be missing. Drops are counted separately.
CREATE VIEW subscription_analytics.collector_gaps AS
SELECT gap_start, gap_end, gap_end - gap_start AS gap_length, ended_clean
FROM (
    SELECT at AS gap_start,
           lead(at) OVER (ORDER BY at, run_id, seq) AS gap_end,
           kind = 'stop' AS ended_clean
    FROM subscription_analytics.collector_heartbeats
) g
WHERE gap_end - gap_start > interval '3 minutes';

-- Successful usage by hour. Token sums cover only attempts with known usage.
CREATE VIEW subscription_analytics.usage_hourly AS
SELECT date_trunc('hour', ended_at)                          AS hour,
       account_hash, provider, auth_kind,
       coalesce(served_model, upstream_model)                AS model,
       count(*)                                              AS attempts,
       count(*) FILTER (WHERE NOT usage_known)               AS usage_unknown_attempts,
       count(*) FILTER (WHERE accounting_quality <> 'complete') AS accounting_not_complete,
       sum(total_tokens)                                     AS total_tokens,
       sum(input_uncached_tokens)                            AS input_uncached_tokens,
       sum(input_cache_read_tokens)                          AS input_cache_read_tokens,
       sum(input_cache_write_tokens)                         AS input_cache_write_tokens,
       sum(raw_cache_creation_5m_tokens)                     AS cache_write_5m_tokens,
       sum(raw_cache_creation_1h_tokens)                     AS cache_write_1h_tokens,
       sum(output_non_reasoning_tokens)                      AS output_non_reasoning_tokens,
       sum(output_reasoning_tokens)                          AS output_reasoning_tokens,
       sum(unclassified_tokens)                              AS unclassified_tokens
FROM subscription_analytics.attempts
WHERE NOT failed
GROUP BY 1, 2, 3, 4, 5;

CREATE VIEW subscription_analytics.usage_daily AS
SELECT date_trunc('day', hour) AS day, account_hash, provider, auth_kind, model,
       sum(attempts) AS attempts, sum(usage_unknown_attempts) AS usage_unknown_attempts,
       sum(accounting_not_complete) AS accounting_not_complete,
       sum(total_tokens) AS total_tokens, sum(input_uncached_tokens) AS input_uncached_tokens,
       sum(input_cache_read_tokens) AS input_cache_read_tokens, sum(input_cache_write_tokens) AS input_cache_write_tokens,
       sum(cache_write_5m_tokens) AS cache_write_5m_tokens, sum(cache_write_1h_tokens) AS cache_write_1h_tokens,
       sum(output_non_reasoning_tokens) AS output_non_reasoning_tokens,
       sum(output_reasoning_tokens) AS output_reasoning_tokens, sum(unclassified_tokens) AS unclassified_tokens
FROM subscription_analytics.usage_hourly
GROUP BY 1, 2, 3, 4, 5;

-- Failures by day and category. failure_class is the existing auth classifier verdict.
CREATE VIEW subscription_analytics.error_categories_daily AS
SELECT date_trunc('day', coalesce(ended_at, received_at)) AS day,
       provider, auth_kind, origin, failure_class, classifier_version, http_status, error_type, error_code,
       count(*) AS attempts
FROM subscription_analytics.attempts
WHERE failed
GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9;

-- One row per pair of consecutive observations of one account and window.
-- Traffic: attempts of that account with ended_at in (prev_observed_at, observed_at].
-- pairing:
--   'same_window'     both resets known and within 120 s of each other, no reset passed
--   'reset_passed'    the previous reset time is at or before this observation
--   'missing_reset'   one or both resets unknown: the window identity cannot be proved
--   'different_reset' resets differ by more than 120 s
-- ratio_eligible: same_window, and not 'fable_weekly' (that limit covers only some
-- models, but the traffic sums cover all models of the account).
-- Negative and zero deltas stay eligible: headers can arrive out of order or with
-- other precision, so spans use the NET change (sum of deltas), which does not
-- count the same percentage point twice. Per-interval ratios need delta > 0.
-- Percentages are exact numeric values from the reported text (raw_value), not
-- the stored double: 0.07 (fraction) * 100 must equal a reported 7 (percent),
-- or a float residue such as 8.9e-16 would become a false positive change.
-- used_percent / prev_used_percent / delta_percent below are these numerics.
CREATE VIEW subscription_analytics.quota_intervals AS
WITH gaps AS MATERIALIZED (
    SELECT gap_start, gap_end FROM subscription_analytics.collector_gaps
), canon AS (
    SELECT o.*,
           CASE WHEN o.raw_value ~ '^[0-9]+(\.[0-9]+)?$'
                THEN o.raw_value::numeric * CASE WHEN o.raw_scale = 'fraction' THEN 100 ELSE 1 END
                -- Other text forms: the stored value, rounded to 6 decimals.
                ELSE round(o.used_percent::numeric, 6)
           END AS pct
    FROM subscription_analytics.quota_observations o
), obs AS (
    SELECT canon.*,
           lag(observed_at)  OVER w AS prev_observed_at,
           lag(pct)          OVER w AS prev_pct,
           lag(reset_at)     OVER w AS prev_reset_at,
           lag(source)       OVER w AS prev_source
    FROM canon
    WINDOW w AS (PARTITION BY account_hash, window_key ORDER BY observed_at, observation_id)
), paired AS (
    SELECT obs.*,
           pct - prev_pct AS delta_percent,
           CASE
               WHEN prev_reset_at IS NOT NULL AND prev_reset_at <= observed_at THEN 'reset_passed'
               WHEN reset_at IS NULL OR prev_reset_at IS NULL THEN 'missing_reset'
               WHEN abs(extract(epoch FROM reset_at - prev_reset_at)) <= 120 THEN 'same_window'
               ELSE 'different_reset'
           END AS pairing
    FROM obs
    WHERE prev_observed_at IS NOT NULL
)
SELECT p.account_hash, p.provider, p.auth_kind, p.window_key, p.observation_id,
       p.prev_observed_at, p.observed_at,
       p.prev_source, p.source, p.observed_at_quality,
       p.prev_pct AS prev_used_percent, p.pct AS used_percent, p.delta_percent,
       p.prev_reset_at, p.reset_at, p.pairing,
       (p.pairing = 'same_window' AND p.window_key <> 'fable_weekly') AS ratio_eligible,
       (p.delta_percent < 0)                                          AS negative_delta,
       (p.delta_percent = 0)                                          AS zero_delta,
       -- Smallest step the reports can show, in percentage points (NULL for polls).
       CASE WHEN p.raw_decimals IS NULL THEN NULL
            WHEN p.raw_scale = 'fraction' THEN 100 * power(10::numeric, -p.raw_decimals)
            ELSE power(10::numeric, -p.raw_decimals) END             AS report_step_pp,
       t.attempts, t.successful_attempts, t.failed_attempts, t.usage_unknown_attempts,
       f.inflight_at_start, f.inflight_at_end, m.missing_end_attempts, u.no_account_attempts_same_provider,
       t.total_tokens, t.input_uncached_tokens, t.input_cache_read_tokens, t.input_cache_write_tokens,
       t.output_non_reasoning_tokens, t.output_reasoning_tokens, t.unclassified_tokens,
       (p.delta_percent > 0 AND t.attempts = 0)                       AS percent_moved_without_traffic,
       EXISTS (SELECT 1 FROM gaps g
               WHERE g.gap_start < p.observed_at AND g.gap_end > p.prev_observed_at) AS collector_gap_overlap,
       (p.observed_at_quality = 'attempt_end')                        AS observed_at_upper_bound,
       CASE WHEN p.delta_percent > 0 THEN t.successful_attempts / p.delta_percent END AS successful_attempts_per_pp,
       CASE WHEN p.delta_percent > 0 THEN t.attempts / p.delta_percent END            AS attempted_per_pp,
       CASE WHEN p.delta_percent > 0 THEN t.total_tokens / p.delta_percent END        AS total_tokens_per_pp
FROM paired p
CROSS JOIN LATERAL (
    -- Uses attempts_account_ended_idx.
    SELECT count(*)                                         AS attempts,
           count(*) FILTER (WHERE NOT a.failed)             AS successful_attempts,
           count(*) FILTER (WHERE a.failed)                 AS failed_attempts,
           count(*) FILTER (WHERE NOT a.usage_known AND NOT a.failed) AS usage_unknown_attempts,
           coalesce(sum(a.total_tokens), 0)                 AS total_tokens,
           coalesce(sum(a.input_uncached_tokens), 0)        AS input_uncached_tokens,
           coalesce(sum(a.input_cache_read_tokens), 0)      AS input_cache_read_tokens,
           coalesce(sum(a.input_cache_write_tokens), 0)     AS input_cache_write_tokens,
           coalesce(sum(a.output_non_reasoning_tokens), 0)  AS output_non_reasoning_tokens,
           coalesce(sum(a.output_reasoning_tokens), 0)      AS output_reasoning_tokens,
           coalesce(sum(a.unclassified_tokens), 0)          AS unclassified_tokens
    FROM subscription_analytics.attempts a
    WHERE a.account_hash = p.account_hash
      AND a.ended_at > p.prev_observed_at AND a.ended_at <= p.observed_at
) t
CROSS JOIN LATERAL (
    -- Attempts that cross an edge. Only attempts that end within 2 hours after the
    -- edge are checked, so the scan stays bounded.
    SELECT count(*) FILTER (WHERE a.started_at < p.prev_observed_at
                            AND a.ended_at > p.prev_observed_at
                            AND a.ended_at <= p.prev_observed_at + interval '2 hours') AS inflight_at_start,
           count(*) FILTER (WHERE a.started_at < p.observed_at
                            AND a.ended_at > p.observed_at) AS inflight_at_end
    FROM subscription_analytics.attempts a
    WHERE a.account_hash = p.account_hash
      AND a.ended_at > p.prev_observed_at
      AND a.ended_at <= greatest(p.prev_observed_at, p.observed_at) + interval '2 hours'
) f
CROSS JOIN LATERAL (
    -- Attempts with no end time cannot be placed; counted by receive time.
    -- Uses attempts_no_end_idx.
    SELECT count(*) AS missing_end_attempts
    FROM subscription_analytics.attempts a
    WHERE a.ended_at IS NULL AND a.account_hash = p.account_hash
      AND a.received_at > p.prev_observed_at AND a.received_at <= p.observed_at
) m
CROSS JOIN LATERAL (
    -- Attempts with no account cannot be placed on any account. Uses attempts_no_account_idx.
    SELECT count(*) AS no_account_attempts_same_provider
    FROM subscription_analytics.attempts a
    WHERE a.account_hash IS NULL AND a.provider = p.provider
      AND a.ended_at > p.prev_observed_at AND a.ended_at <= p.observed_at
) u;

-- Continuous runs of ratio-eligible intervals of one account and window. A
-- reset, a missing or different reset, or the fable window ends a span; that
-- traffic is in no span. delta_percent is the net change over the span.
-- Ratios exist only when the net change is positive. The main served ratio is
-- successful_attempts_per_pp; attempted_per_pp also counts failed retries.
CREATE VIEW subscription_analytics.capacity_spans AS
WITH marked AS (
    SELECT q.*,
           sum(CASE WHEN ratio_eligible THEN 0 ELSE 1 END)
               OVER (PARTITION BY account_hash, window_key ORDER BY observed_at, observation_id
                     ROWS UNBOUNDED PRECEDING) AS span_no
    FROM subscription_analytics.quota_intervals q
), spans AS (
    SELECT account_hash, window_key, span_no,
           min(provider)                      AS provider,
           min(auth_kind)                     AS auth_kind,
           min(prev_observed_at)              AS span_start,
           max(observed_at)                   AS span_end,
           min(reset_at)                      AS reset_at,
           count(*)                           AS intervals,
           count(*) FILTER (WHERE zero_delta)     AS zero_delta_intervals,
           count(*) FILTER (WHERE negative_delta) AS negative_delta_intervals,
           sum(delta_percent)                 AS delta_percent,
           max(report_step_pp)                AS report_step_pp,
           sum(attempts)                      AS attempts,
           sum(successful_attempts)           AS successful_attempts,
           sum(failed_attempts)               AS failed_attempts,
           sum(usage_unknown_attempts)        AS usage_unknown_attempts,
           sum(missing_end_attempts)          AS missing_end_attempts,
           sum(inflight_at_start)             AS boundary_inflight_attempts,
           sum(no_account_attempts_same_provider) AS no_account_attempts_same_provider,
           sum(total_tokens)                  AS total_tokens,
           sum(input_uncached_tokens)         AS input_uncached_tokens,
           sum(input_cache_read_tokens)       AS input_cache_read_tokens,
           sum(input_cache_write_tokens)      AS input_cache_write_tokens,
           sum(output_non_reasoning_tokens)   AS output_non_reasoning_tokens,
           sum(output_reasoning_tokens)       AS output_reasoning_tokens,
           sum(unclassified_tokens)           AS unclassified_tokens,
           bool_or(collector_gap_overlap)     AS collector_gap_overlap,
           bool_or(percent_moved_without_traffic) AS percent_moved_without_traffic,
           bool_or(observed_at_upper_bound)   AS has_upper_bound_timestamps
    FROM marked
    WHERE ratio_eligible
    GROUP BY account_hash, window_key, span_no
)
SELECT spans.*,
       CASE WHEN delta_percent > 0 THEN successful_attempts / delta_percent END         AS successful_attempts_per_pp,
       CASE WHEN delta_percent > 0 THEN attempts / delta_percent END                    AS attempted_per_pp,
       CASE WHEN delta_percent > 0 THEN total_tokens / delta_percent END                AS total_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN input_uncached_tokens / delta_percent END       AS input_uncached_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN input_cache_read_tokens / delta_percent END     AS input_cache_read_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN input_cache_write_tokens / delta_percent END    AS input_cache_write_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN output_non_reasoning_tokens / delta_percent END AS output_non_reasoning_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN output_reasoning_tokens / delta_percent END     AS output_reasoning_tokens_per_pp
FROM spans;

-- Model mix inside each span. The denominator is shared by all models of the
-- account, so there is no per-model ratio here, only shares.
CREATE VIEW subscription_analytics.capacity_span_model_mix AS
SELECT s.account_hash, s.window_key, s.span_no,
       coalesce(a.served_model, a.upstream_model) AS model,
       count(*)                                   AS attempts,
       count(*) FILTER (WHERE NOT a.failed)       AS successful_attempts,
       sum(a.total_tokens)                        AS total_tokens,
       count(*)::double precision / nullif(s.attempts, 0) AS attempt_share
FROM subscription_analytics.capacity_spans s
JOIN subscription_analytics.attempts a
  ON a.account_hash = s.account_hash AND a.ended_at > s.span_start AND a.ended_at <= s.span_end
GROUP BY s.account_hash, s.window_key, s.span_no, s.attempts, coalesce(a.served_model, a.upstream_model);

-- Daily trend: net change and traffic of the eligible intervals that ended that day.
CREATE VIEW subscription_analytics.capacity_daily AS
WITH days AS (
    SELECT date_trunc('day', observed_at) AS day, account_hash, window_key,
           min(provider) AS provider, min(auth_kind) AS auth_kind,
           count(*) AS intervals,
           count(*) FILTER (WHERE zero_delta) AS zero_delta_intervals,
           count(*) FILTER (WHERE negative_delta) AS negative_delta_intervals,
           sum(delta_percent) AS delta_percent,
           sum(attempts) AS attempts,
           sum(successful_attempts) AS successful_attempts,
           sum(usage_unknown_attempts) AS usage_unknown_attempts,
           sum(total_tokens) AS total_tokens,
           sum(input_uncached_tokens) AS input_uncached_tokens,
           sum(input_cache_read_tokens) AS input_cache_read_tokens,
           sum(input_cache_write_tokens) AS input_cache_write_tokens,
           sum(output_non_reasoning_tokens) AS output_non_reasoning_tokens,
           sum(output_reasoning_tokens) AS output_reasoning_tokens,
           bool_or(collector_gap_overlap) AS collector_gap_overlap
    FROM subscription_analytics.quota_intervals
    WHERE ratio_eligible
    GROUP BY 1, 2, 3
)
SELECT days.*,
       CASE WHEN delta_percent > 0 THEN successful_attempts / delta_percent END         AS successful_attempts_per_pp,
       CASE WHEN delta_percent > 0 THEN attempts / delta_percent END                    AS attempted_per_pp,
       CASE WHEN delta_percent > 0 THEN total_tokens / delta_percent END                AS total_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN input_uncached_tokens / delta_percent END       AS input_uncached_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN input_cache_read_tokens / delta_percent END     AS input_cache_read_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN input_cache_write_tokens / delta_percent END    AS input_cache_write_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN output_non_reasoning_tokens / delta_percent END AS output_non_reasoning_tokens_per_pp,
       CASE WHEN delta_percent > 0 THEN output_reasoning_tokens / delta_percent END     AS output_reasoning_tokens_per_pp
FROM days;
