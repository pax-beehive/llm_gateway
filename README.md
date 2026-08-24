# Universal LLM Gateway

A Responses-first, multi-tenant Go gateway implementing [ADR 0001](docs/adr/0001-build-universal-llm-gateway-core.md). It exposes OpenAI-compatible Responses and Chat Completions APIs, normalizes provider events behind capability-aware Model Routes, and persists lifecycle state with tenant-scoped CAS revisions.

## Run locally

The smallest local mode uses the deterministic echo Provider and an in-memory Response Store:

```sh
make run-dev
```

Create a Response:

```sh
curl -sS http://localhost:8080/v1/responses \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"echo-v1","input":"hello gateway"}'
```

Stream Chat Completions:

```sh
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"model":"echo-v1","stream":true,"messages":[{"role":"user","content":"hello stream"}]}'
```

Use `docker compose up --build` for the PostgreSQL-backed development stack. The Compose credentials and bearer token are development-only.

## Production configuration

Production requires PostgreSQL and refuses to start without an explicit synchronous multi-AZ durability attestation:

```text
DATABASE_URL=postgres://...
GATEWAY_ENV=production
GATEWAY_DURABILITY_ATTESTATION=sync-multi-az
GATEWAY_API_KEYS_JSON={"secret-token":"tenant-id"}
GATEWAY_TENANT_HOME_REGIONS_JSON={"tenant-id":"us-west"}
GATEWAY_ROUTES_JSON=[...]
```

Each route declares its public model, provider model, execution/home region, credential scope, versioned Capability Profile, prices, and secret environment-variable reference. Credentials are read server-side and are never returned by the API.

Example route:

```json
[
  {
    "id": "provider-us-primary",
    "provider": "provider-name",
    "public_model": "gateway-model",
    "provider_model": "provider-model-id",
    "base_url": "https://provider.example/v1",
    "api_key_env": "PROVIDER_API_KEY",
    "region": "us-west",
    "home_region": "us-west",
    "credential_scope": "tenant-primary",
    "capability_revision": 1,
    "capabilities": {
      "text": "native",
      "streaming": "native",
      "sampling": "native",
      "tools": "translated"
    },
    "input_cost_per_million": 1.0,
    "output_cost_per_million": 4.0
  }
]
```

Strict compatibility is the default. A feature classified as `translated` is considered only when the request sets `compatibility_mode` to `best_effort`; `unsupported` or undeclared features are rejected instead of silently dropped.

## Verification

```sh
make test
make test-race
make vet
```

The core migration is embedded in the binary and runs only when `GATEWAY_MIGRATE=true`. Production schema rollout should normally happen as a separate deployment gate.
