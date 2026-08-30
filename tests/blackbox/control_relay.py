#!/usr/bin/env python3
import base64
import hashlib
import hmac
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("CONTROL_PLANE_URL", "http://127.0.0.1:8081").rstrip("/")
TOKEN = os.environ.get("CONTROL_PLANE_TOKEN", "local-control-admin-token")
KEY = os.environ.get(
    "GATEWAY_CONTROL_RELAY_HMAC_KEY",
    "local-development-gateway-hmac-key-0001",
).encode()
GATEWAY_ID = "gateway-local"


def admin(method, path, body, idempotency_key):
    payload = json.dumps(body).encode()
    request = urllib.request.Request(BASE + path, data=payload, method=method)
    request.add_header("Authorization", "Bearer " + TOKEN)
    request.add_header("Content-Type", "application/json")
    request.add_header("X-Request-ID", "relay-blackbox-" + str(time.time_ns()))
    request.add_header("Idempotency-Key", idempotency_key)
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read())


def gateway_authorization(method, request_uri):
    timestamp = str(int(time.time()))
    digest = hashlib.sha256(b"").hexdigest()
    canonical = f"{GATEWAY_ID}\n{timestamp}\n{method}\n{request_uri}\n{digest}".encode()
    signature = hmac.new(KEY, canonical, hashlib.sha256).hexdigest()
    return f"Gateway-HMAC {GATEWAY_ID}:{timestamp}:{signature}"


def gateway_get(path, expected=200):
    request = urllib.request.Request(BASE + path, method="GET")
    request.add_header("Authorization", gateway_authorization("GET", path))
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            payload = json.loads(response.read())
            assert response.status == expected, (response.status, payload)
            return payload, dict(response.headers)
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            payload = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            payload = raw.decode(errors="replace")
        assert error.code == expected, (error.code, payload)
        return payload, dict(error.headers)


suffix = str(time.time_ns())
connection_id = "pc-relay-blackbox-" + suffix
secret = "relay-blackbox-provider-secret"
registered = admin(
    "POST",
    "/control/v1/provider-connections",
    {
        "id": connection_id,
        "provider": "openai",
        "display_name": "Relay black-box OpenAI",
        "base_url": "https://api.openai.com/v1",
        "region": "local",
        "credential_scope": "relay-blackbox",
        "secret": secret,
        "capability_declaration": {"revision": 1, "features": {"text": "native"}},
        "reason": "register relay black-box connection",
    },
    "register-" + connection_id,
)
enabled = admin(
    "POST",
    f"/control/v1/provider-connections/{connection_id}/enable",
    {"expected_revision": registered["revision"], "reason": "enable relay black-box connection"},
    "enable-" + connection_id,
)

cursor = 0
projected = None
for _ in range(100):
    path = "/internal/v1/control-events?" + urllib.parse.urlencode(
        {"after": cursor, "limit": 256}
    )
    batch, headers = gateway_get(path)
    assert headers.get("Cache-Control") == "no-store", headers
    assert batch["source_head"] >= batch["next_cursor"] >= cursor, batch
    for event in batch["events"]:
        if event["aggregate_id"] == connection_id and event["aggregate_revision"] == enabled["revision"]:
            projected = event
    next_cursor = batch["next_cursor"]
    if projected or next_cursor == cursor:
        break
    cursor = next_cursor

assert projected is not None, "schema-3 Provider Connection event was not relayed"
assert projected["schema_version"] == 3, projected
encoded = json.dumps(projected)
assert secret not in encoded and "secret_ref" not in encoded, projected

bootstrap, headers = gateway_get("/internal/v1/control-bootstrap")
assert headers.get("Cache-Control") == "no-store", headers
assert bootstrap["schema_version"] == 1 and bootstrap["source_cursor"] >= cursor, bootstrap
bootstrap_connection = next(
    item for item in bootstrap["provider_connections"] if item["connection_id"] == connection_id
)
assert bootstrap_connection["revision"] == enabled["revision"], bootstrap_connection
encoded_bootstrap = json.dumps(bootstrap)
assert secret not in encoded_bootstrap and "secret_ref" not in encoded_bootstrap, bootstrap

secret_path = f"/internal/v1/provider-connection-secrets/{connection_id}?" + urllib.parse.urlencode(
    {"credential_version": enabled["credential_version"], "revision": enabled["revision"]}
)
delivered, headers = gateway_get(secret_path)
assert headers.get("Cache-Control") == "no-store", headers
assert delivered["connection_id"] == connection_id
assert delivered["revision"] == enabled["revision"]
assert delivered["credential_version"] == enabled["credential_version"]
assert base64.b64decode(delivered["material"]).decode() == secret

stale_path = f"/internal/v1/provider-connection-secrets/{connection_id}?" + urllib.parse.urlencode(
    {"credential_version": enabled["credential_version"], "revision": enabled["revision"] + 1}
)
gateway_get(stale_path, expected=404)
print("PASS authenticated relay, multi-projection bootstrap, and exact-version secret delivery")
