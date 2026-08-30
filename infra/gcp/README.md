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
