output "artifact_repository" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.gateway.repository_id}"
}

output "build_service_account" {
  value = google_service_account.build.id
}

output "build_source_bucket" {
  value = google_storage_bucket.build_source.name
}

output "cloud_sql_instance" {
  value = google_sql_database_instance.gateway.connection_name
}

output "database_name" {
  value = google_sql_database.gateway.name
}

output "migration_service_account" {
  value = google_service_account.migration.email
}

output "role_config_service_account" {
  value = google_service_account.role_config.email
}

output "gateway_service_account" {
  value = google_service_account.gateway.email
}

output "control_service_account" {
  value = google_service_account.control.email
}

output "metering_service_account" {
  value = google_service_account.metering.email
}

output "metering_export_bucket" {
  value = google_storage_bucket.metering_exports.name
}

output "github_workload_identity_provider" {
  value = google_iam_workload_identity_pool_provider.github_actions.name
}

output "deploy_service_account" {
  value = google_service_account.deploy.email
}
