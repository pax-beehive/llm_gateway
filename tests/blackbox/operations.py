#!/usr/bin/env python3
import hashlib
import hmac
import json
import os
import time
import urllib.error
import urllib.request

BASE = os.environ.get("CONTROL_PLANE_URL", "http://127.0.0.1:8081").rstrip("/")
PATH = "/internal/v1/operations/gateway-observations"
KEY = b"local-development-gateway-hmac-key-0001"


def call(method, path, body=None, authorization="Bearer local-control-admin-token", expected=(200,)):
    payload = None if body is None else json.dumps(body, separators=(",", ":")).encode()
    request = urllib.request.Request(BASE + path, data=payload, method=method)
    if authorization:
        request.add_header("Authorization", authorization)
    if payload is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            status, data = response.status, json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as error:
        status, data = error.code, json.loads(error.read() or b"{}")
    assert status in expected, (method, path, status, data)
    return status, data


def authorization(body_bytes, timestamp=None):
    timestamp = int(time.time()) if timestamp is None else timestamp
    digest = hashlib.sha256(body_bytes).hexdigest()
    canonical = f"gateway-local\n{timestamp}\nPOST\n{PATH}\n{digest}".encode()
    signature = hmac.new(KEY, canonical, hashlib.sha256).hexdigest()
    return f"Gateway-HMAC gateway-local:{timestamp}:{signature}"


status, ready = call("GET", "/readyz", authorization=None)
assert ready["ready"] is True, ready

now = int(time.time())
observation = {
    "event_schema_version": 2,
    "gateway_id": "gateway-local",
    "region": "local",
    "build_sha": "blackbox-build",
    "database_schema_version": 21,
    "routing_catalog_revision": 3,
    "access_projection_revision": 4,
    "execution_epoch_floor": 1,
    "last_usage_outbox_id": 0,
    "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - 60)),
    "observed_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now)),
    "consumers": [{"name": "routing_catalog", "lag_seconds": 0, "pending_count": 0}],
    "backlogs": {"outbox_pending_count": 0},
}
body = json.dumps(observation, separators=(",", ":")).encode()
request = urllib.request.Request(BASE + PATH, data=body, method="POST")
request.add_header("Authorization", authorization(body))
request.add_header("Content-Type", "application/json")
with urllib.request.urlopen(request, timeout=10) as response:
    assert response.status == 202, response.status

_, gateway = call("GET", "/control/v1/operations/gateways/gateway-local")
assert gateway["gateway_id"] == "gateway-local" and gateway["heartbeat_status"] == "current", gateway
for path in ("gateways", "publications", "outbox", "consumers", "jobs"):
    call("GET", f"/control/v1/operations/{path}")

tampered = dict(observation)
tampered["gateway_id"] = "gateway-forged"
call("POST", PATH, tampered, authorization(body), expected=(401,))
print("PASS operations readiness, authenticated heartbeat, monotonic query surfaces")
