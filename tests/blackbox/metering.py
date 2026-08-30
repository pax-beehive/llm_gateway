import json
import os
import urllib.error
import urllib.parse
import urllib.request
import time

BASE = os.environ.get("METERING_BASE_URL", "http://127.0.0.1:8082")
TOKEN = os.environ.get("METERING_TOKEN", "local-metering-admin-token")
TENANT = os.environ.get("METERING_TENANT_ID", "tenant-dev")


def call(method, path, body=None, auth=True):
    payload = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(BASE + path, data=payload, method=method)
    if auth:
        request.add_header("Authorization", "Bearer " + TOKEN)
    if payload is not None:
        request.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(request, timeout=5) as response:
        return response.status, dict(response.headers), json.loads(response.read())


status, _, ready = call("GET", "/readyz", auth=False)
assert status == 200 and ready["ready"] is True

tenant = urllib.parse.quote(TENANT, safe="")
status, _, summary = call("GET", f"/metering/v1/tenants/{tenant}/usage")
assert status == 200 and summary["totals"] is not None

status, _, export = call("POST", f"/metering/v1/usage/exports?tenant_id={tenant}")
assert status == 202 and export["status"] == "queued"

download = None
for _ in range(50):
    status, headers, current = call("GET", f"/metering/v1/usage/exports/{export['id']}?tenant_id={tenant}")
    assert status == 200
    if current["status"] == "succeeded":
        download = headers["Link"].split(";", 1)[0].strip("<>")
        break
    assert current["status"] in ("queued", "running")
    time.sleep(0.1)
assert download is not None

with urllib.request.urlopen(BASE + download, timeout=5) as response:
    csv_payload = response.read().decode()
    assert response.status == 200 and response.headers["Content-Type"] == "text/csv"
    header = csv_payload.splitlines()[0]
    assert "event_id" in header and "correction_actor_id" in header
    assert "metadata" not in header and "provider_usage" not in header and "credential" not in header

status, _, operations = call("GET", f"/metering/v1/operations/status?tenant_id={tenant}")
assert status == 200 and "projection_cutoff" in operations
print(f"PASS metering tenant={TENANT} export={export['id']} downloaded=true")
