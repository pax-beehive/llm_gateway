# GCP deployment

This Terraform stack owns only resources labeled `application=llm-gateway` and
named with the `llm-gateway-<environment>` prefix. In `pax-fde-prod` it uses the
separate state prefix `llm-gateway/prod` and must not import or modify the
existing `fde-platform` resources.

The first apply creates the isolated foundation: required APIs, Artifact
Registry, build and Metering buckets, service accounts, least-privilege IAM,
Secret Manager entries, and a regional HA Cloud SQL instance. After the
release images exist, the same stack creates schema-migration and database-role
configuration Cloud Run Jobs pinned by digest. It does not create public DNS,
a load balancer, or long-running Cloud Run services.

```sh
cp infra/gcp/backend.hcl.example infra/gcp/backend.hcl
terraform -chdir=infra/gcp init -backend-config=backend.hcl
cp infra/gcp/terraform.tfvars.example infra/gcp/release.auto.tfvars
# Replace both example image values with immutable Artifact Registry digests.
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
act as their exact service accounts. It cannot read Terraform state or Secret
Manager payloads.

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
role configuration. The workflow records source, build, digest, and execution
evidence in the GitHub job summary.

Long-running Gateway, Control Plane, and Metering services remain outside this
release stage until their Terraform resources and readiness/bootstrap gates are
implemented. Their images are built and recorded, but no service traffic is
changed by this workflow.
