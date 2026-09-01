/**
 * Control-plane types for the Tenants console. Field names mirror
 * internal/core/types.go (TenantPolicy, APIKeyPolicy, QuotaLimits),
 * internal/controlapi/server.go (tenantResponse, credentialResource),
 * internal/tenantadmin + internal/credentialadmin (PolicyRevision), and
 * internal/quota/snapshot.go (EnforcementSnapshot).
 */

export interface Paged<T> {
  data: T[];
  next_cursor?: string;
}

export interface QuotaLimits {
  max_input_tokens?: number;
  max_output_tokens?: number;
  max_cost_micros?: number;
  requests_per_minute?: number;
  tokens_per_minute?: number;
  daily_spend_micros?: number;
  monthly_spend_micros?: number;
  refresh_daily_spend_micros?: number;
  refresh_monthly_spend_micros?: number;
  embedding_input_units?: number;
  rerank_documents?: number;
  capability_spend_micros?: number;
  currency?: string;
}

export interface TenantPolicy {
  revision?: number;
  max_concurrent_responses?: number;
  max_input_items?: number;
  allow_stored_responses?: boolean;
  allow_cache_protection?: boolean;
  allow_content_inspection?: boolean;
  retention_seconds?: number;
  limits?: QuotaLimits;
}

export interface APIKeyPolicy {
  revision?: number;
  allow_cache_protection?: boolean;
  allow_content_inspection?: boolean;
  allowed_public_models?: string[] | null;
  allowed_operations?: string[] | null;
  allowed_cidrs?: string[] | null;
  allowed_regions?: string[] | null;
  max_concurrent_responses?: number | null;
  limits?: QuotaLimits;
}

export type TenantStatus = "active" | "suspended" | "closed";

export interface Tenant {
  id: string;
  slug: string;
  display_name: string;
  status: TenantStatus | string;
  home_region: string;
  execution_epoch: number;
  policy: TenantPolicy;
  metadata?: Record<string, unknown>;
  revision: number;
}

export type APIKeyStatus = "active" | "revoked";

/** credentialResource map from internal/controlapi/server.go. */
export interface GatewayKey {
  id: string;
  tenant_id: string;
  name: string;
  prefix: string;
  digest_version: string;
  status: APIKeyStatus | string;
  revision: number;
  policy: APIKeyPolicy;
  metadata?: Record<string, unknown>;
  expires_at?: string | null;
  revoked_at?: string | null;
  predecessor_id?: string | null;
  replacement_id?: string | null;
  grace_expires_at?: string | null;
}

/** 201 response of issue — carries the raw secret exactly once. */
export interface IssuedKey extends GatewayKey {
  secret?: string;
}

/** 201 response of rotate. */
export interface RotateKeyResult {
  predecessor: GatewayKey;
  replacement: GatewayKey;
  secret?: string;
}

export interface TenantPolicyRevision {
  tenant_id: string;
  revision: number;
  policy: TenantPolicy;
  actor_type: string;
  actor_id: string;
  change_reason: string;
  created_at: string;
}

export interface KeyPolicyRevision {
  tenant_id: string;
  api_key_id: string;
  revision: number;
  policy: APIKeyPolicy;
  actor_type: string;
  actor_id: string;
  change_reason: string;
  created_at: string;
}

export interface TenantEffectivePolicy {
  tenant_policy: TenantPolicy;
  limits: QuotaLimits;
}

export interface KeyEffectivePolicy {
  tenant_policy: TenantPolicy;
  api_key_policy: APIKeyPolicy;
  limits: QuotaLimits;
  max_concurrent_responses: number;
}

export interface QuotaBalance {
  limit?: number;
  reserved: number;
  committed: number;
  uncertain: number;
  remaining?: number;
}

export interface QuotaSnapshot {
  tenant_id: string;
  api_key_id?: string;
  tenant_policy_revision: number;
  api_key_policy_revision?: number;
  currency?: string;
  observed_at: string;
  balances: Record<string, QuotaBalance>;
}
