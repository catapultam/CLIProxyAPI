-- Least-privilege grants. Apply as admin after schema.sql and views.sql.
-- Create the roles first, with passwords kept out of this file, for example:
--   CREATE ROLE cliproxy_analytics_writer LOGIN PASSWORD '...';
--   CREATE ROLE cliproxy_analytics_reader LOGIN PASSWORD '...';

-- Collector: insert only. It uses ON CONFLICT DO NOTHING without a conflict target,
-- which needs no SELECT or UPDATE.
GRANT USAGE ON SCHEMA subscription_analytics TO cliproxy_analytics_writer;
GRANT INSERT ON subscription_analytics.attempts,
                subscription_analytics.quota_observations,
                subscription_analytics.collector_heartbeats
    TO cliproxy_analytics_writer;

-- Dashboards: read only.
GRANT USAGE ON SCHEMA subscription_analytics TO cliproxy_analytics_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA subscription_analytics TO cliproxy_analytics_reader;
