#!/usr/bin/env python3
import json
import os
import time
import urllib.error
import urllib.request

base = os.environ.get("CONTROL_PLANE_URL", "http://127.0.0.1:8081").rstrip("/")
token = os.environ.get("CONTROL_PLANE_TOKEN", "local-control-admin-token")
connection_id = f"pc-blackbox-{time.time_ns()}"
secret = "blackbox-provider-secret"


def call(method, path, body=None, idem=None, etag=None, expected=(200,)):
    payload = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + path, data=payload, method=method)
    request.add_header("Authorization", "Bearer " + token)
    request.add_header("X-Request-ID", "provider-blackbox-" + str(time.time_ns()))
    if body is not None:
        request.add_header("Content-Type", "application/json")
    if idem:
        request.add_header("Idempotency-Key", idem)
    if etag:
        request.add_header("If-Match", etag)
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


status, headers, registered = call("POST", "/control/v1/provider-connections", {
    "id": connection_id,
    "provider": "openai",
    "display_name": "Black-box OpenAI",
    "base_url": "https://api.openai.com/v1",
    "region": "us-test",
    "credential_scope": "blackbox",
    "secret": secret,
    "capability_declaration": {"revision": 1, "features": {"text": "native"}},
    "reason": "black-box registration",
}, "register-" + connection_id, expected=(201,))
assert status == 201 and registered["revision"] == 1 and registered["administrative_status"] == "disabled"
assert secret not in json.dumps(registered) and "secret_ref" not in registered

_, _, page = call("GET", f"/control/v1/provider-connections?provider=openai&region=us-test&limit=100")
assert any(item["id"] == connection_id for item in page["data"])

_, headers, enabled = call("POST", f"/control/v1/provider-connections/{connection_id}/enable", {
    "expected_revision": 1, "reason": "enable black-box connection",
}, "enable-" + connection_id, etag='"1"')
assert enabled["revision"] == 2 and enabled["administrative_status"] == "enabled"


def run_operation(path, body, key):
    status, headers, operation = call("POST", path, body, key, etag=f'"{body["expected_revision"]}"', expected=(202,))
    assert status == 202 and operation["status"] == "queued"
    assert secret not in json.dumps(operation) and "pending_secret_ref" not in operation
    for _ in range(80):
        _, _, current = call("GET", headers["Location"])
        if current["status"] in ("succeeded", "failed", "uncertain"):
            assert current["status"] == "succeeded", current
            return current
        time.sleep(0.05)
    raise AssertionError("operation did not complete")


probe = run_operation(f"/control/v1/provider-connections/{connection_id}/probes", {
    "expected_revision": 2, "reason": "deterministic probe",
}, "probe-" + connection_id)
assert probe["result"]["observed_status"] == "healthy"

discovery = run_operation(f"/control/v1/provider-connections/{connection_id}/model-discoveries", {
    "expected_revision": 2, "reason": "deterministic discovery",
}, "discovery-" + connection_id)
assert discovery["result"]["model_count"] == 2
models_path = f"/control/v1/provider-operations/{discovery['id']}/models"
_, _, first = call("GET", models_path + "?limit=1")
assert len(first["data"]) == 1 and first["next_cursor"]
_, _, second = call("GET", models_path + "?limit=1&cursor=" + first["next_cursor"])
assert len(second["data"]) == 1 and not second.get("next_cursor")
assert first["data"][0]["id"] != second["data"][0]["id"]
assert secret not in json.dumps(first)
call("GET", models_path + "?limit=101", expected=(400,))
call("GET", models_path + "?cursor=invalid!", expected=(400,))
call("GET", f"/control/v1/provider-operations/{probe['id']}/models", expected=(400,))

rotation = run_operation(f"/control/v1/provider-connections/{connection_id}/credential-rotations", {
    "expected_revision": 2, "secret": "rotated-blackbox-provider-secret", "reason": "black-box rotation",
}, "rotation-" + connection_id)
assert rotation["result"]["credential_version"] == 2 and rotation["result"]["connection_revision"] == 3

_, _, disabled = call("POST", f"/control/v1/provider-connections/{connection_id}/disable", {
    "expected_revision": 3, "reason": "disable black-box connection",
}, "disable-" + connection_id, etag='"3"')
assert disabled["administrative_status"] == "disabled" and disabled["revision"] == 4

print(f"PASS provider-connection id={connection_id} revision=4 operations=probe,discovery,rotation")
