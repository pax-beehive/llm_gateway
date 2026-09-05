import test from "node:test";
import assert from "node:assert/strict";
import { configureModel, dollarsToMicros, modalityWarning, needsModelSetup, setupProblems } from "../src/pages/models/setup";
import { hasValidationReport, type Draft, type ManagedRoute } from "../src/pages/routing/types";
import type { ProviderConnection } from "../src/pages/providers/types";

const connection: ProviderConnection = { id: "pc-test", provider: "openai", display_name: "Test", base_url: "https://example.invalid", region: "us-west", credential_scope: "test", administrative_status: "enabled", capability_declaration: { revision: 4, features: { text: "native", streaming: "native" } }, credential_version: 1, revision: 7, created_at: "", updated_at: "" };
const route: ManagedRoute = { route_id: "route-test", public_model: "text-example", provider_model: "text-example", provider_connection_id: connection.id, execution_region: "us-west", home_region: "us-west", capability_profile_revision: 1, capabilities: {}, administrative_status: "disabled", selection_policy: { priority: 100, weight: 1 }, tenant_visibility_policy: { all_tenants: false, tenant_ids: [] }, cache_usage_reliable: false, cache_protection_policy: { enabled: false }, provider_cost_snapshot: { id: "", provider: "openai", model: "text-example", region: "us-west", currency: "USD", input_per_million_micros: 0, output_per_million_micros: 0, cached_input_per_million_micros: 0, cache_write_per_million_micros: 0 } };
const settings = { include: true, textConfirmed: true, streaming: false, input: "1.25", output: "10", cached: "0.125", source: "contract-2026" };

test("failed validation reports remain visible, new and edited drafts have no stale report", () => {
  const draft = { status: "draft", validation_hash: "failure-hash", validation_report: { valid: false, hash: "failure-hash", errors: [{ code: "price_snapshot_required", path: "routes[0]", message: "Missing price" }], warnings: [] } } as Draft;
  assert.equal(hasValidationReport(draft), true);
  assert.equal(hasValidationReport({ ...draft, validation_hash: "", validation_report: { ...draft.validation_report, hash: "", errors: [] } }), false);
});
test("prices preserve USD precision and reject blank, negative, non-finite or unsafe values", () => {
  assert.equal(dollarsToMicros("1.25"), 1250000);
  assert.equal(dollarsToMicros("0.000001"), 1);
  assert.equal(dollarsToMicros("0"), 0);
  assert.equal(dollarsToMicros("9007199254.740991"), Number.MAX_SAFE_INTEGER);
  assert.equal(dollarsToMicros("9007199254.740992"), null);
  for (const value of ["", " ", "-1", "NaN", "Infinity", "1e3", "0.0000001", "999999999999999"]) assert.equal(dollarsToMicros(value), null);
});
test("inventory alone cannot establish capabilities or prices", () => {
  assert.equal(needsModelSetup(route), true);
  assert.ok(setupProblems(route, { ...settings, textConfirmed: false, input: "", source: "" }, connection).length >= 3);
  assert.throws(() => configureModel(route, { ...settings, input: "" }, connection, { all_tenants: true }));
  assert.ok(setupProblems(route, settings, { ...connection, administrative_status: "disabled" }).length);
  assert.ok(setupProblems(route, settings, { ...connection, capability_declaration: { revision: 4, features: { text: "translated" } } }).length);
});
test("known non-chat models cannot be activated by the text setup; unknown names require confirmation", () => {
  for (const id of ["gpt-image-2", "gpt-realtime-2.1-mini", "gpt-audio-mini", "text-embedding-3-small", "whisper-1", "dall-e-3", "sora-2"]) {
    assert.ok(modalityWarning(id));
    assert.ok(setupProblems({ ...route, provider_model: id }, settings, connection).length);
  }
  assert.equal(modalityWarning("new-unknown-model"), null);
  assert.ok(setupProblems(route, { ...settings, textConfirmed: false }, connection).length);
  assert.deepEqual(setupProblems({ ...route, provider_model: "gpt-image-2" }, { ...settings, include: false }, connection), []);
});
test("configured draft has explicit text support, immutable price evidence and unchanged tenant policy revisions", () => {
  const policy = { all_tenants: false, tenant_ids: ["tn-example"], limit_policy_revisions: { "tn-example": 8 } };
  const result = configureModel(route, settings, connection, policy);
  assert.equal(needsModelSetup(result), false);
  assert.equal(result.administrative_status, "active");
  assert.deepEqual(result.capabilities, { text: "native" });
  assert.equal(result.capability_profile_revision, 4);
  assert.equal(result.provider_cost_snapshot.input_per_million_micros, 1250000);
  assert.equal(result.provider_cost_snapshot.cached_input_per_million_micros, 125000);
  assert.equal(result.provider_cost_snapshot.source, "contract-2026");
  assert.match(result.provider_cost_snapshot.id, /^price-/);
  assert.ok(result.provider_cost_snapshot.effective_at! > 0);
  assert.deepEqual(result.tenant_visibility_policy, policy);
  assert.notEqual(result.tenant_visibility_policy, policy);
  assert.equal(route.administrative_status, "disabled");
  assert.equal(route.provider_cost_snapshot.id, "");
  assert.throws(() => configureModel(route, settings, connection, { all_tenants: false }));
});
test("streaming needs explicit model confirmation and connection support", () => {
  assert.deepEqual(configureModel(route, { ...settings, streaming: true }, connection, { all_tenants: true }).capabilities, { text: "native", streaming: "native" });
  assert.ok(setupProblems(route, { ...settings, streaming: true }, { ...connection, capability_declaration: { revision: 4, features: { text: "native" } } }).length);
});
