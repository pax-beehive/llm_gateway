locals {
  github_repository_id       = "1349076388"
  github_repository_owner_id = "292671531"
}

resource "google_iam_workload_identity_pool" "github_actions" {
  project                   = var.project_id
  workload_identity_pool_id = "${local.prefix}-github"
  display_name              = "LLM Gateway ${var.environment} GitHub Actions"
  description               = "Short-lived deployment identity for pax-beehive/llm_gateway main releases."
}

resource "google_iam_workload_identity_pool_provider" "github_actions" {
  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.github_actions.workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  display_name                       = "pax-beehive llm_gateway main"

  attribute_mapping = {
    "google.subject"                = "assertion.sub"
    "attribute.repository_id"       = "assertion.repository_id"
    "attribute.repository_owner_id" = "assertion.repository_owner_id"
    "attribute.ref"                 = "assertion.ref"
  }
  attribute_condition = "assertion.repository_id == '${local.github_repository_id}' && assertion.repository_owner_id == '${local.github_repository_owner_id}' && assertion.ref == 'refs/heads/main'"

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account" "deploy" {
  project      = var.project_id
  account_id   = "${local.prefix}-deploy"
  display_name = "LLM Gateway ${var.environment} GitHub release deployer"
  description  = "Updates release image digests and executes gated Cloud Run Jobs; no Terraform state or secret payload access."
}

resource "google_project_iam_custom_role" "release_deployer" {
  project     = var.project_id
  role_id     = "llmGatewayReleaseDeployer"
  title       = "LLM Gateway Release Deployer"
  description = "Submit and observe builds, execute release Jobs, and update existing private services."
  permissions = [
    "cloudbuild.builds.create",
    "cloudbuild.builds.get",
    "cloudbuild.locations.get",
    "run.executions.get",
    "run.jobs.get",
    "run.jobs.run",
    "run.jobs.update",
    "run.operations.get",
    "run.revisions.get",
    "run.revisions.list",
    "run.services.get",
    "run.services.update",
  ]
}

resource "google_project_iam_member" "release_deployer" {
  project = var.project_id
  role    = google_project_iam_custom_role.release_deployer.name
  member  = "serviceAccount:${google_service_account.deploy.email}"
}

resource "google_project_iam_member" "release_service_usage" {
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = "serviceAccount:${google_service_account.deploy.email}"
}

resource "google_service_account_iam_member" "github_actions_deploy" {
  service_account_id = google_service_account.deploy.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github_actions.name}/attribute.repository_id/${local.github_repository_id}"
}

resource "google_service_account_iam_member" "release_act_as" {
  for_each = {
    build       = google_service_account.build.name
    control     = google_service_account.control.name
    gateway     = google_service_account.gateway.name
    metering    = google_service_account.metering.name
    bff         = google_service_account.bff.name
    migration   = google_service_account.migration.name
    role_config = google_service_account.role_config.name
  }

  service_account_id = each.value
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.deploy.email}"
}

resource "google_artifact_registry_repository_iam_member" "release_reader" {
  project    = var.project_id
  location   = google_artifact_registry_repository.gateway.location
  repository = google_artifact_registry_repository.gateway.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.deploy.email}"
}

resource "google_storage_bucket_iam_member" "release_source" {
  bucket = google_storage_bucket.build_source.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.deploy.email}"
}

resource "google_storage_bucket_iam_member" "release_source_bucket_reader" {
  bucket = google_storage_bucket.build_source.name
  role   = "roles/storage.legacyBucketReader"
  member = "serviceAccount:${google_service_account.deploy.email}"
}
