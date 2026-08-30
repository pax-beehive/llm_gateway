\set ON_ERROR_STOP on
BEGIN;
CREATE ROLE llm_gateway_metering_test NOLOGIN;
\set metering_role llm_gateway_metering_test
\ir ../../scripts/postgres/configure-metering-role.sql
DO $$
BEGIN
    IF NOT has_table_privilege('llm_gateway_metering_test','transactional_outbox','SELECT')
       OR NOT has_column_privilege('llm_gateway_metering_test','transactional_outbox','published_at','UPDATE')
       OR has_column_privilege('llm_gateway_metering_test','transactional_outbox','payload','UPDATE')
       OR NOT has_table_privilege('llm_gateway_metering_test','metering_inbox','INSERT')
       OR has_table_privilege('llm_gateway_metering_test','usage_ledger','SELECT')
       OR has_table_privilege('llm_gateway_metering_test','responses','SELECT')
       OR has_table_privilege('llm_gateway_metering_test','metering_usage_facts','DELETE') THEN
        RAISE EXCEPTION 'Metering role isolation assertion failed';
    END IF;
END $$;
ROLLBACK;
