import type { ManagedRoute, TenantVisibilityPolicy } from "../routing/types";
import type { ProviderConnection } from "../providers/types";

// A name can warn about a different modality, but never proves text support.
export function modalityWarning(model: string): string | null {
  return /(^|[-_])(image|audio|realtime|embedding|embeddings|moderation|tts|whisper|dall-e|sora)([-_\d]|$)/i.test(model)
    ? "This appears to be an image, audio, realtime or other non-chat model. It needs a separate capability workflow; remove it from this text-model draft."
    : null;
}

export function needsModelSetup(route: ManagedRoute): boolean {
  const price = route.provider_cost_snapshot;
  return !Object.keys(route.capabilities ?? {}).length || !price.id || !price.source || !price.effective_at ||
    (!route.tenant_visibility_policy.all_tenants && !route.tenant_visibility_policy.tenant_ids?.length);
}

export interface ModelSettings {
  include: boolean;
  textConfirmed: boolean;
  streaming: boolean;
  input: string;
  output: string;
  cached: string;
  source: string;
}

export function dollarsToMicros(value: string): number | null {
  if (!/^\d+(\.\d{1,6})?$/.test(value.trim())) return null;
  const [whole, fraction = ""] = value.trim().split(".");
  const micros = BigInt(whole) * 1_000_000n + BigInt(fraction.padEnd(6, "0"));
  return micros <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(micros) : null;
}

export function setupProblems(route: ManagedRoute, settings: ModelSettings, connection?: ProviderConnection): string[] {
  if (!settings.include) return [];
  const errors: string[] = [];
  const warning = modalityWarning(route.provider_model);
  if (warning) errors.push(warning);
  if (!connection) errors.push("Provider connection is unavailable. Reload and try again.");
  else {
    if (connection.administrative_status !== "enabled") errors.push("Enable this provider connection first.");
    if (connection.capability_declaration.features.text !== "native") errors.push("The connection must declare native text support.");
    if (settings.streaming && connection.capability_declaration.features.streaming !== "native") errors.push("This connection does not declare native streaming support.");
  }
  if (!settings.textConfirmed) errors.push("Confirm this model supports native text requests.");
  if ([settings.input, settings.output, settings.cached].some(v => dollarsToMicros(v) === null)) errors.push("Enter input, output and cached-input prices in USD per million tokens (0 is allowed when explicitly intended).");
  if (!settings.source.trim()) errors.push("Add the price source, such as a provider pricing URL or your contract reference.");
  return errors;
}

export function configureModel(route: ManagedRoute, settings: ModelSettings, connection: ProviderConnection, visibility: TenantVisibilityPolicy): ManagedRoute {
  if (setupProblems(route, settings, connection).length) throw new Error("Model setup is incomplete");
  if (!visibility.all_tenants && !visibility.tenant_ids?.length) throw new Error("Choose tenant visibility");
  return {
    ...route,
    administrative_status: "active",
    capability_profile_revision: connection.capability_declaration.revision,
    capabilities: { text: "native", ...(settings.streaming ? { streaming: "native" as const } : {}) },
    tenant_visibility_policy: structuredClone(visibility),
    cache_usage_reliable: false,
    cache_protection_policy: { enabled: false },
    provider_cost_snapshot: {
      ...route.provider_cost_snapshot,
      id: `price-${crypto.randomUUID()}`, provider: connection.provider, model: route.provider_model,
      region: route.execution_region, currency: "USD", source: settings.source.trim(), effective_at: Math.floor(Date.now() / 1000),
      input_per_million_micros: dollarsToMicros(settings.input)!, output_per_million_micros: dollarsToMicros(settings.output)!,
      cached_input_per_million_micros: dollarsToMicros(settings.cached)!, cache_write_per_million_micros: 0,
    },
  };
}
