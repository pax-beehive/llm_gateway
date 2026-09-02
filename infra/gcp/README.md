# GCP deployment

This Terraform stack owns only resources labeled `application=llm-gateway` and
named with the `llm-gateway-<environment>` prefix. In `pax-fde-prod` it uses the
separate state prefix `llm-gateway/prod` and must not import or modify the
existing `fde-platform` resources.

The first apply creates the isolated foundation: required APIs, Artifact
Registry, build and Metering buckets, service accounts, least-privilege IAM,
Secret Manager entries, and a regional HA Cloud SQL instance. After the
release images exist, the same stack creates schema-migration and database-role
configuration Cloud Run Jobs pinned by digest, followed by private Control
Plane and Metering Cloud Run services. The Gateway resource remains gated by
`gateway_service_enabled=false` until a Provider Connection, Routing Catalog,
Tenant, and Gateway API key have been published. This stage does not create
public DNS or a load balancer unless `console_domain` is set for an enabled
BFF service.

## Production console domain

Setting `console_domain` creates a fixed global IPv4 address, a global external
Application Load Balancer backed by a regional serverless NEG, HTTP-to-HTTPS
redirects, and a Certificate Manager DNS authorization. Keep
`console_certificate_enabled=false` for this first apply. Publish the output
authorization CNAME as DNS-only and point the console hostname at the
`console_domain.ipv4_address` output. After both records resolve publicly, set
`console_certificate_enabled=true` and apply again to create the managed
certificate and HTTPS frontend. The A record may remain DNS-only while the
Google certificate reaches `ACTIVE`; if a Cloudflare proxy is enabled afterward,
retain the authorization CNAME as DNS-only and use Full (strict) origin TLS.
If a failed authorization must be retried or a certificate must be replaced,
increment `console_certificate_generation`; Terraform creates the new
certificate before switching the map entry and removing the previous one.

Apply the infrastructure and DNS records before changing `bff_public_url` to
the custom HTTPS origin. That ordering keeps the existing `run.app` login flow
available until DNS and TLS are ready. WorkOS must allow both the custom-domain
callback (`/api/auth/callback`) and logout URL before the BFF is rolled to the
new public URL.

Pin `bff_workos_api_key_version` to the numeric Secret Manager version used by
production. During a key rotation, add and validate the new secret version,
advance this variable, roll the BFF, and only then disable the previous version.

```sh
terraform -chdir=infra/gcp output -json console_domain
gcloud certificate-manager certificates describe llm-gateway-prod-console \
  --project=pax-fde-prod --location=global
```

The services use an isolated regional VPC and Direct VPC egress with
`ALL_TRAFFIC`. Private Google Access makes same-project Cloud Run calls count
as internal and keeps Google API access on Google's network; Cloud NAT provides
the controlled outbound path required by future LLM Provider calls. The
`10.90.0.0/24` launch subnet covers the bounded first-region revision overlap
and must be revisited with measured scaling before raising service maxima. NAT
logs retain errors only so egress failures remain diagnosable without recording
every Provider connection.

Before setting `gateway_service_enabled=true`, run the immutable
`gateway-bootstrap` image as a one-shot private Cloud Run Job with the Control
Plane service account and database role. It creates the bounded
`tenant-gateway-canary` Tenant, issues a one-time Gateway API key into a
pre-created Secret Manager secret, and publishes the single-Tenant
`gpt-5.6-luna` Routing Catalog revision. The canary limits each request to
4,096 input tokens, 256 output tokens, and USD 0.005, with USD 0.10 daily and
USD 1.00 monthly spend ceilings. The Job is idempotent and refuses to replace
an unrelated active Routing Catalog.

```sh
cp infra/gcp/backend.hcl.example infra/gcp/backend.hcl
terraform -chdir=infra/gcp init -backend-config=backend.hcl
cp infra/gcp/terraform.tfvars.example infra/gcp/release.auto.tfvars
# Replace all example image values with immutable Artifact Registry digests.
terraform -chdir=infra/gcp plan -out=/tmp/llm-gateway-foundation.tfplan
terraform -chdir=infra/gcp apply /tmp/llm-gateway-foundation.tfplan
```

Database passwords and machine keys are generated into the encrypted,
versioned private Terraform state and copied only into Secret Manager. Never
print `terraform show`, secret versions, database URLs, or generated values in
deployment logs.

Run the migration job first, then the role-configuration job. Both operations
are idempotent, but a failed execution must be inspected before it is retried.

## Main-branch release CI/CD

The foundation stack also creates a GitHub Workload Identity Federation pool,
provider, and a dedicated release service account. Trust is restricted to the
immutable `pax-beehive/llm_gateway` repository and owner IDs on
`refs/heads/main`. The release identity can submit and observe Cloud Builds,
read image metadata, update the two existing Cloud Run Jobs, execute them, and
update existing private services by immutable digest. It can act only as the
build, Job, and runtime service accounts. It cannot read Terraform state or
Secret Manager payloads, create a service, change service IAM, or open ingress.

GitHub's `production` environment must require a reviewer and permit only the
`main` deployment branch. Configure these non-secret repository variables from
Terraform outputs and stable foundation names:

```text
GCP_PROJECT_ID=pax-fde-prod
GCP_REGION=us-west1
GCP_WORKLOAD_IDENTITY_PROVIDER=<github_workload_identity_provider output>
GCP_DEPLOY_SERVICE_ACCOUNT=<deploy_service_account output>
GCP_ARTIFACT_REPOSITORY=us-west1-docker.pkg.dev/pax-fde-prod/llm-gateway-prod-containers
GCP_BUILD_SERVICE_ACCOUNT=llm-gateway-prod-build@pax-fde-prod.iam.gserviceaccount.com
GCP_BUILD_SOURCE_BUCKET=pax-fde-prod-llm-gateway-prod-build-source
```

After CI succeeds for a push to `main`, `.github/workflows/deploy-gcp.yml`
waits for production approval, checks out the exact tested SHA, authenticates
without a service-account key, builds all release images once, resolves their
digests, updates and runs schema migration, and then updates and runs database
role configuration. It then rolls Control Plane, Metering, and—once
provisioned—Gateway, requiring each new revision to become Ready. The workflow
records source, build, digest, execution, and revision evidence in the GitHub
job summary.

All three services use `INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER`; no direct
public `run.app` bypass is accepted. Control Plane grants `roles/run.invoker`
only to the exact Gateway and Metering identities. Those callers also attach
their existing application HMAC in `Authorization` and an ADC-backed Cloud Run
ID token in `X-Serverless-Authorization`.

Before the Cloudflare Access identity adapter is configured, Control Plane and
Metering set their human IAM verifier to explicit deny-all. Their health and
readiness endpoints remain available to Cloud Run, while every human
management assertion fails closed. Provider live operations also remain
disabled, so this stage cannot spend against an LLM Provider.

## Provider credential bootstrap

The first production Provider credentials enter through the one-shot
`provider-bootstrap` image. An operator creates a temporary Secret Manager
bundle containing exactly the four allowlisted Provider keys, runs an ephemeral
Cloud Run Job as the Control Plane service account on the runtime VPC, and then
deletes both the Job and temporary bundle. The command registers four disabled
Provider Connections through the normal audited domain service, enables only
the explicitly selected canary, and performs one zero-spend-bounded model-list
probe. The resulting `llm-gateway-pc-*` secrets use immutable versions and are
the only credentials retained after cleanup. Never pass the bundle as a plain
environment value, command argument, Terraform value, or CI output.

The initial max-instance and concurrency settings are protective launch bounds,
not capacity claims. Replace them only with load-test and budget evidence.
