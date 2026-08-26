# Local verification

## Fast deterministic checks

```sh
make test
make test-race
make vet
make build
```

The Makefile sets `GOCACHE=/tmp/llm_gateway-go-cache` by default because the normal cache may be unwritable in sandboxed environments.

## PostgreSQL integration

```sh
make test-integration-local
```

This starts only the Compose PostgreSQL dependency, supplies `TEST_DATABASE_URL`, and runs integration-tagged access, store, configuration, cache, quota, HTTP, and command packages. Use `make integration-down` when the user asks to stop it; stopping preserves the named volume.

Do not substitute an all-skipped test run. `make test-integration` intentionally fails if `TEST_DATABASE_URL` is missing.

## Local Gateway modes

Deterministic in-memory echo mode:

```sh
make run-dev
```

PostgreSQL-backed Compose stack:

```sh
make compose-up
```

The repository README contains development-only example credentials. Do not reuse them outside local development and do not print any real replacement value.

## Public black boxes

With a target Gateway running:

```sh
make test-openai-sdk-blackbox
make test-stage-a-blackbox
make test-codex-sandbox-blackbox
```

The SDK suite covers Models, Responses lifecycle and chaining, streaming, Chat, and Conversations. The Stage A suite uses the deterministic route to cover the capability catalog, embeddings, moderations, and reranking without Provider spend. The Codex suite uses a deterministic local Provider mock and requires every `exec resume` to retain `workspace-write` sandbox permission.

## Provider conformance

Normal Provider adapter tests are offline and use local HTTP transports. Live checks are separate:

```sh
make test-live-providers
make test-live-provider-tools
```

Run live targets only when explicitly requested. They require the ignored `.env`, incur Provider calls, and must remain narrow. Tool conformance is a separate higher-cost gate from basic text streaming.

## Evidence report

For implementation completion, report:

```text
source SHA and worktree
unit tests
race tests
vet
build
PostgreSQL integration
HTTP/SDK black box
Codex black box when compatibility changed
live Provider proof only when actually run
```

Passing local gates is not deployment proof.
