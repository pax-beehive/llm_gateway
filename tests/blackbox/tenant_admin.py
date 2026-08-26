#!/usr/bin/env python3
"""Black-box Tenant Administration contract without direct database access."""

from __future__ import annotations

import json
import os
import time
import urllib.request


BASE_URL = os.getenv("CONTROL_PLANE_BASE_URL", "http://127.0.0.1:8081").rstrip("/")
TOKEN = os.getenv("CONTROL_PLANE_TOKEN", "local-control-admin-token")


def request(method: str, path: str, payload=None, headers=None):
    body = None if payload is None else json.dumps(payload).encode()
    request_headers = {
        "Authorization": "Bearer " + TOKEN,
        "Content-Type": "application/json",
        "X-Request-ID": "blackbox-" + str(time.time_ns()),
    }
    request_headers.update(headers or {})
    call = urllib.request.Request(
        BASE_URL + path, data=body, method=method, headers=request_headers
    )
    with urllib.request.urlopen(call, timeout=10) as response:
        return response.status, dict(response.headers), json.load(response)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def main() -> None:
    suffix = str(time.time_ns())
    tenant_id = "blackbox-tenant-" + suffix
    create_payload = {
        "id": tenant_id,
        "slug": tenant_id,
        "display_name": "Black-box Tenant",
        "home_region": "local",
        "metadata": {"suite": "tenant-admin-blackbox"},
        "initial_policy": {"revision": 1, "max_concurrent_responses": 1},
        "reason": "black-box provisioning",
    }
    status, headers, tenant = request(
        "POST",
        "/control/v1/tenants",
        create_payload,
        {"Idempotency-Key": "create-" + suffix},
    )
    require(status == 201, f"create status={status}")
    require(headers.get("Etag") == '"1"', f"create ETag={headers.get('Etag')!r}")
    require(tenant["id"] == tenant_id and tenant["revision"] == 1, f"created={tenant!r}")

    issue_payload = {
        "name": "black-box workload",
        "metadata": {"suite": "tenant-admin-blackbox"},
        "policy": {"revision": 1},
        "reason": "black-box credential issuance",
    }
    status, _, issued = request(
        "POST",
        f"/control/v1/tenants/{tenant_id}/gateway-api-keys",
        issue_payload,
        {"Idempotency-Key": "issue-" + suffix},
    )
    require(status == 201 and issued.get("secret", "").startswith("gw_"), f"issued={issued!r}")
    status, replay_headers, replayed = request(
        "POST",
        f"/control/v1/tenants/{tenant_id}/gateway-api-keys",
        issue_payload,
        {"Idempotency-Key": "issue-" + suffix},
    )
    require(status == 201 and replayed["id"] == issued["id"], f"replayed={replayed!r}")
    require("secret" not in replayed, "idempotent replay revealed the raw Gateway API Key")
    require(replay_headers.get("Idempotency-Replayed") == "true", f"replay headers={replay_headers!r}")

    status, headers, policy = request(
        "PUT",
        f"/control/v1/tenants/{tenant_id}/policy",
        {
            "policy": {"revision": 2, "max_concurrent_responses": 4},
            "reason": "black-box policy publication",
        },
        {"Idempotency-Key": "policy-" + suffix, "If-Match": '"1"'},
    )
    require(status == 200, f"policy status={status}")
    require(headers.get("Etag") == '"2"', f"policy ETag={headers.get('Etag')!r}")
    require(policy["revision"] == 2, f"policy={policy!r}")

    status, headers, suspended = request(
        "POST",
        f"/control/v1/tenants/{tenant_id}/transitions",
        {"target": "suspended", "reason": "black-box lifecycle transition"},
        {"Idempotency-Key": "suspend-" + suffix, "If-Match": '"2"'},
    )
    require(status == 200, f"suspend status={status}")
    require(headers.get("Etag") == '"3"', f"suspend ETag={headers.get('Etag')!r}")
    require(suspended["status"] == "suspended" and suspended["revision"] == 3, f"suspended={suspended!r}")

    _, _, observed = request("GET", f"/control/v1/tenants/{tenant_id}")
    require(observed["status"] == "suspended", f"observed={observed!r}")
    _, _, revisions = request("GET", f"/control/v1/tenants/{tenant_id}/policy-revisions")
    require([item["revision"] for item in revisions["data"]] == [1, 2], f"revisions={revisions!r}")
    print(f"PASS tenant-admin tenant={tenant_id} row_revision=3 policy_revision=2")


if __name__ == "__main__":
    main()
