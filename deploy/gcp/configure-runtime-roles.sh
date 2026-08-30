#!/bin/sh
set -eu

: "${ADMIN_DATABASE_URL:?ADMIN_DATABASE_URL is required}"
: "${TENANT_ADMIN_DB_ROLE:?TENANT_ADMIN_DB_ROLE is required}"
: "${GATEWAY_DB_ROLE:?GATEWAY_DB_ROLE is required}"
: "${METERING_DB_ROLE:?METERING_DB_ROLE is required}"
: "${ROLE_CONFIGURATION_CONFIRM:?ROLE_CONFIGURATION_CONFIRM is required}"

if [ "$ROLE_CONFIGURATION_CONFIRM" != "apply" ]; then
  echo "ROLE_CONFIGURATION_CONFIRM=apply is required" >&2
  exit 2
fi

psql "$ADMIN_DATABASE_URL" \
  -v tenant_admin_role="$TENANT_ADMIN_DB_ROLE" \
  -v gateway_role="$GATEWAY_DB_ROLE" \
  -f /opt/llm-gateway/configure-tenant-admin-roles.sql

psql "$ADMIN_DATABASE_URL" \
  -v metering_role="$METERING_DB_ROLE" \
  -f /opt/llm-gateway/configure-metering-role.sql

echo "runtime database roles configured"
