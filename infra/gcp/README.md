# GCP deployment

This Terraform stack owns only resources labeled `application=llm-gateway` and
named with the `llm-gateway-<environment>` prefix. In `pax-fde-prod` it uses the
separate state prefix `llm-gateway/prod` and must not import or modify the
existing `fde-platform` resources.

The first apply creates the isolated foundation: required APIs, Artifact
Registry, build and Metering buckets, service accounts, least-privilege IAM,
Secret Manager entries, and a regional HA Cloud SQL instance. It does not
create public DNS, a load balancer, or Cloud Run services.

```sh
cp infra/gcp/backend.hcl.example infra/gcp/backend.hcl
terraform -chdir=infra/gcp init -backend-config=backend.hcl
terraform -chdir=infra/gcp plan -var='project_id=pax-fde-prod' -out=/tmp/llm-gateway-foundation.tfplan
terraform -chdir=infra/gcp apply /tmp/llm-gateway-foundation.tfplan
```

Database passwords and machine keys are generated into the encrypted,
versioned private Terraform state and copied only into Secret Manager. Never
print `terraform show`, secret versions, database URLs, or generated values in
deployment logs.
