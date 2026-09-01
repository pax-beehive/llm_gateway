\set ON_ERROR_STOP on

-- The role must be created by deployment infrastructure. Run as schema owner.
REVOKE ALL PRIVILEGES ON TABLE
    metering_inbox,metering_usage_facts,metering_projection_generations,
    metering_usage_daily,metering_outbox_claims,metering_exports,metering_quota_denials
FROM PUBLIC, :"metering_role";
REVOKE ALL PRIVILEGES ON SEQUENCE metering_projection_generations_id_seq
FROM PUBLIC, :"metering_role";

GRANT SELECT ON TABLE transactional_outbox TO :"metering_role";
GRANT UPDATE (published_at) ON TABLE transactional_outbox TO :"metering_role";
GRANT SELECT,INSERT ON TABLE metering_inbox,metering_usage_facts,metering_quota_denials TO :"metering_role";
GRANT SELECT,INSERT,UPDATE ON TABLE metering_projection_generations,metering_usage_daily,
    metering_outbox_claims,metering_exports TO :"metering_role";
GRANT DELETE ON TABLE metering_usage_daily,metering_outbox_claims TO :"metering_role";
GRANT USAGE,SELECT ON SEQUENCE metering_projection_generations_id_seq TO :"metering_role";

-- Controlled ledger bootstrap is deliberately not granted to the runtime
-- role. Execute it through a separate, time-bounded bootstrap role.
