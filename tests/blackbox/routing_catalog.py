#!/usr/bin/env python3
import json
import os
import time
import urllib.error
import urllib.request

base = os.environ.get("CONTROL_PLANE_URL", "http://127.0.0.1:8081").rstrip("/")
token = os.environ.get("CONTROL_PLANE_TOKEN", "local-control-admin-token")
suffix = str(time.time_ns())
connection_id = "pc-routing-blackbox-" + suffix
draft_id = "rcd-blackbox-" + suffix


def call(method, path, body=None, idem=None, expected=(200,)):
    payload = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + path, data=payload, method=method)
    request.add_header("Authorization", "Bearer " + token)
    request.add_header("X-Request-ID", "routing-blackbox-" + str(time.time_ns()))
    if body is not None:
        request.add_header("Content-Type", "application/json")
    if idem:
        request.add_header("Idempotency-Key", idem)
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            raw = response.read()
            if response.status not in expected:
                raise AssertionError((response.status, raw))
            return response.status, dict(response.headers), json.loads(raw or b"{}")
    except urllib.error.HTTPError as error:
        raw = error.read()
        if error.code not in expected:
            raise AssertionError((error.code, raw)) from error
        return error.code, dict(error.headers), json.loads(raw or b"{}")


status, _, current = call("GET", "/control/v1/routing-catalog", expected=(200, 404))
base_revision = current["revision"] if status == 200 else 0

_, _, connection = call("POST", "/control/v1/provider-connections", {
    "id": connection_id,
    "provider": "openai",
    "display_name": "Routing black-box OpenAI",
    "base_url": "https://api.openai.com/v1",
    "region": "us-test",
    "credential_scope": "routing-blackbox",
    "secret": "routing-blackbox-secret",
    "capability_declaration": {"revision": 1, "features": {"text": "native", "streaming": "native"}},
    "reason": "register Routing Catalog dependency",
}, "register-" + connection_id, expected=(201,))
_, _, connection = call("POST", f"/control/v1/provider-connections/{connection_id}/enable", {
    "expected_revision": connection["revision"], "reason": "enable Routing Catalog dependency",
}, "enable-" + connection_id)

document = {"routes": [{
    "route_id": "route-blackbox-" + suffix,
    "public_model": "blackbox-model-" + suffix,
    "provider_connection_id": connection_id,
    "provider_model": "provider-model-blackbox",
    "execution_region": "us-test",
    "home_region": "us-test",
    "capability_profile_revision": 1,
    "capabilities": {"text": "native", "streaming": "native"},
    "provider_cost_snapshot": {
        "id": "price-blackbox-" + suffix,
        "provider": "openai",
        "model": "provider-model-blackbox",
        "region": "us-test",
        "currency": "USD",
        "input_per_million_micros": 1_000_000,
        "cached_input_per_million_micros": 100_000,
        "cache_write_per_million_micros": 0,
        "output_per_million_micros": 4_000_000,
        "effective_at": int(time.time()),
        "source": "black-box-contract",
    },
    "administrative_status": "active",
    "selection_policy": {"priority": 10, "weight": 100, "max_concurrency": 2, "sticky_routing_eligible": True},
    "tenant_visibility_policy": {"all_tenants": True},
    "cache_usage_reliable": True,
    "cache_protection_policy": {"enabled": False},
}]}

_, _, draft = call("POST", "/control/v1/routing-catalog/drafts", {
    "id": draft_id, "base_revision": base_revision, "document": document, "reason": "create black-box catalog",
}, "create-" + draft_id, expected=(201,))
_, _, draft = call("POST", f"/control/v1/routing-catalog/drafts/{draft_id}/validate", {
    "expected_revision": draft["revision"], "reason": "validate black-box catalog",
})
assert draft["status"] == "validated" and draft["validation_report"]["valid"]

_, _, probes = call("POST", f"/control/v1/routing-catalog/drafts/{draft_id}/probe", {
    "expected_revision": draft["revision"], "reason": "probe black-box catalog",
}, "probe-" + draft_id, expected=(202,))
assert len(probes["data"]) == 1 and probes["data"][0]["connection_id"] == connection_id

_, headers, result = call("POST", f"/control/v1/routing-catalog/drafts/{draft_id}/publish", {
    "expected_revision": draft["revision"], "required_regions": [], "reason": "publish black-box catalog",
}, "publish-" + draft_id, expected=(202,))
publication = result["publication"]
revision = result["revision"]
assert revision["revision"] == base_revision + 1 and publication["catalog_revision"] == revision["revision"]
_, _, publication_status = call("GET", headers["Location"])
assert publication_status["status"] == "published"
_, _, active = call("GET", "/control/v1/routing-catalog")
assert active["revision"] == revision["revision"] and active["document"] == document

_, _, restored = call("POST", f"/control/v1/routing-catalog/revisions/{revision['revision']}/restore", {
    "expected_head": revision["revision"], "required_regions": [], "reason": "exercise immutable restore",
}, "restore-" + draft_id, expected=(202,))
assert restored["revision"]["revision"] == revision["revision"] + 1
assert restored["revision"]["source_revision"] == revision["revision"]

_, _, history = call("GET", "/control/v1/routing-catalog/revisions?limit=10")
assert any(item["revision"] == restored["revision"]["revision"] for item in history["data"])
print(f"PASS routing-catalog draft={draft_id} published={revision['revision']} restored={restored['revision']['revision']}")
