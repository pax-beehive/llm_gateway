/**
 * Control-plane types for Provider Connections. Field names mirror
 * internal/providerconnection/types.go (ProviderConnection, Operation) and
 * internal/provider/provider.go (CapabilityProfile).
 */

export interface CapabilityProfile {
  revision: number;
  features: Record<string, "native" | "translated" | "unsupported" | string>;
}

export type AdministrativeStatus = "enabled" | "disabled";

export interface ProviderConnection {
  id: string;
  provider: string;
  display_name: string;
  base_url: string;
  region: string;
  credential_scope: string;
  administrative_status: AdministrativeStatus | string;
  capability_declaration: CapabilityProfile;
  credential_version: number;
  revision: number;
  created_at: string;
  updated_at: string;
}

export type OperationType = "probe" | "model_discovery" | "credential_rotation";
export type OperationStatus = "queued" | "running" | "succeeded" | "failed" | "uncertain";

export interface ProviderOperation {
  id: string;
  type: OperationType | string;
  connection_id: string;
  expected_revision: number;
  status: OperationStatus | string;
  result?: Record<string, unknown>;
  error_code?: string;
  error_message?: string;
  created_at: string;
  started_at?: string | null;
  completed_at?: string | null;
}

export interface ProviderOperationPage {
  data: ProviderOperation[];
  next_cursor?: string;
}

export const OPERATION_TYPE_LABEL: Record<string, string> = {
  probe: "Probe",
  model_discovery: "Model discovery",
  credential_rotation: "Credential rotation",
};

export function isTerminalStatus(status: string): boolean {
  return status === "succeeded" || status === "failed" || status === "uncertain";
}
