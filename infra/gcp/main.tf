locals {
  prefix = "llm-gateway-${var.environment}"
  labels = {
    application = "llm-gateway"
    environment = var.environment
    cost_center = "llm-gateway"
    managed_by  = "terraform"
  }
  database_name   = "llm_gateway"
  instance_name   = "${local.prefix}-postgres"
  instance_socket = "/cloudsql/${google_sql_database_instance.gateway.connection_name}"
  database_urls = {
    admin    = "user=llm_gateway_admin password=${random_password.database["admin"].result} dbname=${local.database_name} host=${local.instance_socket}"
    control  = "user=llm_gateway_control password=${random_password.database["control"].result} dbname=${local.database_name} host=${local.instance_socket}"
    gateway  = "user=llm_gateway_runtime password=${random_password.database["gateway"].result} dbname=${local.database_name} host=${local.instance_socket}"
    metering = "user=llm_gateway_metering password=${random_password.database["metering"].result} dbname=${local.database_name} host=${local.instance_socket}"
  }
  cloud_sql_accounts = {
    migration   = google_service_account.migration.email
    role_config = google_service_account.role_config.email
    gateway     = google_service_account.gateway.email
    control     = google_service_account.control.email
    metering    = google_service_account.metering.email
  }
  database_secret_access = {
    migration_admin  = [google_secret_manager_secret.admin_database.id, google_service_account.migration.email]
    role_admin       = [google_secret_manager_secret.admin_database.id, google_service_account.role_config.email]
    gateway_runtime  = [google_secret_manager_secret.gateway_database.id, google_service_account.gateway.email]
    control_runtime  = [google_secret_manager_secret.control_database.id, google_service_account.control.email]
    metering_runtime = [google_secret_manager_secret.metering_database.id, google_service_account.metering.email]
  }
  runtime_secret_access = {
    control_peppers       = [google_secret_manager_secret.api_key_peppers.id, google_service_account.control.email]
    control_gateway_hmac  = [google_secret_manager_secret.control_gateway_hmac_keys.id, google_service_account.control.email]
    control_metering_hmac = [google_secret_manager_secret.control_metering_hmac_keys.id, google_service_account.control.email]
    gateway_peppers       = [google_secret_manager_secret.api_key_peppers.id, google_service_account.gateway.email]
    gateway_control_hmac  = [google_secret_manager_secret.gateway_control_hmac.id, google_service_account.gateway.email]
    metering_operations   = [google_secret_manager_secret.metering_operations_hmac.id, google_service_account.metering.email]
    metering_signing      = [google_secret_manager_secret.metering_export_signing.id, google_service_account.metering.email]
    bff_cookie            = [google_secret_manager_secret.bff_cookie.id, google_service_account.bff.email]
    bff_workos            = [google_secret_manager_secret.bff_workos_api_key.id, google_service_account.bff.email]
  }
}

data "google_project" "current" {
  project_id = var.project_id
}

resource "google_project_service" "required" {
  for_each = toset([
    "artifactregistry.googleapis.com",
    "cloudbuild.googleapis.com",
    "compute.googleapis.com",
    "iamcredentials.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
    "run.googleapis.com",
    "secretmanager.googleapis.com",
    "sqladmin.googleapis.com",
    "storage.googleapis.com",
  ])

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

resource "google_artifact_registry_repository" "gateway" {
  project       = var.project_id
  location      = var.region
  repository_id = "${local.prefix}-containers"
  description   = "Immutable Universal LLM Gateway release images"
  format        = "DOCKER"
  labels        = local.labels

  cleanup_policy_dry_run = false

  cleanup_policies {
    id     = "retain-recent-releases"
    action = "KEEP"
    most_recent_versions {
      keep_count = 20
    }
  }

  cleanup_policies {
    id     = "delete-old-untagged"
    action = "DELETE"
    condition {
      tag_state  = "UNTAGGED"
      older_than = "2592000s"
    }
  }

  depends_on = [google_project_service.required]
}

resource "google_storage_bucket" "build_source" {
  project                     = var.project_id
  name                        = "${var.project_id}-${local.prefix}-build-source"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  labels                      = local.labels

  versioning {
    enabled = true
  }

  lifecycle_rule {
    condition {
      age = 7
    }
    action {
      type = "Delete"
    }
  }

  depends_on = [google_project_service.required]
}

resource "google_storage_bucket" "metering_exports" {
  project                     = var.project_id
  name                        = "${var.project_id}-${local.prefix}-metering-exports"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  labels                      = local.labels

  versioning {
    enabled = true
  }

  retention_policy {
    retention_period = 2592000
  }

  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.required]
}

resource "random_password" "database" {
  for_each = toset(["admin", "control", "gateway", "metering"])
  length   = 40
  special  = false
}

resource "random_password" "api_key_pepper" {
  length  = 48
  special = false
}

resource "random_password" "gateway_control_hmac" {
  length  = 48
  special = false
}

resource "random_password" "metering_operations_hmac" {
  length  = 48
  special = false
}

resource "random_password" "metering_export_signing" {
  length  = 48
  special = false
}

resource "random_password" "bff_cookie" {
  length  = 64
  special = false
}

resource "google_sql_database_instance" "gateway" {
  project          = var.project_id
  name             = local.instance_name
  region           = var.region
  database_version = var.database_version

  deletion_protection = true

  settings {
    tier                        = var.database_tier
    edition                     = "ENTERPRISE"
    availability_type           = "REGIONAL"
    disk_type                   = "PD_SSD"
    disk_size                   = 20
    disk_autoresize             = true
    disk_autoresize_limit       = 200
    deletion_protection_enabled = true
    connector_enforcement       = "REQUIRED"
    user_labels                 = local.labels

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "10:00"
      location                       = var.region
      transaction_log_retention_days = var.pitr_log_days

      backup_retention_settings {
        retained_backups = var.backup_retained_count
        retention_unit   = "COUNT"
      }
    }

    ip_configuration {
      ipv4_enabled = true
    }

    maintenance_window {
      day          = 7
      hour         = 11
      update_track = "stable"
    }
  }

  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.required]
}

resource "google_sql_database" "gateway" {
  project  = var.project_id
  name     = local.database_name
  instance = google_sql_database_instance.gateway.name

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_sql_user" "database" {
  for_each = {
    admin    = "llm_gateway_admin"
    control  = "llm_gateway_control"
    gateway  = "llm_gateway_runtime"
    metering = "llm_gateway_metering"
  }

  project  = var.project_id
  name     = each.value
  instance = google_sql_database_instance.gateway.name
  password = random_password.database[each.key].result
}

resource "google_secret_manager_secret" "admin_database" {
  project   = var.project_id
  secret_id = "${local.prefix}-admin-database-url"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "control_database" {
  project   = var.project_id
  secret_id = "${local.prefix}-control-database-url"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "gateway_database" {
  project   = var.project_id
  secret_id = "${local.prefix}-gateway-database-url"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "metering_database" {
  project   = var.project_id
  secret_id = "${local.prefix}-metering-database-url"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "api_key_peppers" {
  project   = var.project_id
  secret_id = "${local.prefix}-api-key-peppers-json"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "gateway_control_hmac" {
  project   = var.project_id
  secret_id = "${local.prefix}-gateway-control-hmac"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "control_gateway_hmac_keys" {
  project   = var.project_id
  secret_id = "${local.prefix}-control-gateway-hmac-keys-json"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "metering_operations_hmac" {
  project   = var.project_id
  secret_id = "${local.prefix}-metering-operations-hmac"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "control_metering_hmac_keys" {
  project   = var.project_id
  secret_id = "${local.prefix}-control-metering-hmac-keys-json"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "metering_export_signing" {
  project   = var.project_id
  secret_id = "${local.prefix}-metering-export-signing-key"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "bff_cookie" {
  project   = var.project_id
  secret_id = "${local.prefix}-bff-cookie-password"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "bff_workos_api_key" {
  project   = var.project_id
  secret_id = "${local.prefix}-bff-workos-api-key"
  labels    = local.labels
  replication {
    auto {}
  }
  depends_on = [google_project_service.required]
}

data "google_secret_manager_secret" "bff_gateway_api_key" {
  project   = var.project_id
  secret_id = "${local.prefix}-canary-api-key"
}

resource "google_secret_manager_secret_version" "admin_database" {
  secret      = google_secret_manager_secret.admin_database.id
  secret_data = local.database_urls.admin
}

resource "google_secret_manager_secret_version" "control_database" {
  secret      = google_secret_manager_secret.control_database.id
  secret_data = local.database_urls.control
}

resource "google_secret_manager_secret_version" "gateway_database" {
  secret      = google_secret_manager_secret.gateway_database.id
  secret_data = local.database_urls.gateway
}

resource "google_secret_manager_secret_version" "metering_database" {
  secret      = google_secret_manager_secret.metering_database.id
  secret_data = local.database_urls.metering
}

resource "google_secret_manager_secret_version" "api_key_peppers" {
  secret      = google_secret_manager_secret.api_key_peppers.id
  secret_data = jsonencode({ "1" = random_password.api_key_pepper.result })
}

resource "google_secret_manager_secret_version" "gateway_control_hmac" {
  secret      = google_secret_manager_secret.gateway_control_hmac.id
  secret_data = random_password.gateway_control_hmac.result
}

resource "google_secret_manager_secret_version" "control_gateway_hmac_keys" {
  secret      = google_secret_manager_secret.control_gateway_hmac_keys.id
  secret_data = jsonencode({ "gateway-us-west" = random_password.gateway_control_hmac.result })
}

resource "google_secret_manager_secret_version" "metering_operations_hmac" {
  secret      = google_secret_manager_secret.metering_operations_hmac.id
  secret_data = random_password.metering_operations_hmac.result
}

resource "google_secret_manager_secret_version" "control_metering_hmac_keys" {
  secret      = google_secret_manager_secret.control_metering_hmac_keys.id
  secret_data = jsonencode({ "metering-us-west1" = random_password.metering_operations_hmac.result })
}

resource "google_secret_manager_secret_version" "metering_export_signing" {
  secret      = google_secret_manager_secret.metering_export_signing.id
  secret_data = random_password.metering_export_signing.result
}

resource "google_secret_manager_secret_version" "bff_cookie" {
  secret      = google_secret_manager_secret.bff_cookie.id
  secret_data = random_password.bff_cookie.result
}

resource "google_service_account" "build" {
  project      = var.project_id
  account_id   = "${local.prefix}-build"
  display_name = "LLM Gateway ${var.environment} image builder"
}

resource "google_service_account" "migration" {
  project      = var.project_id
  account_id   = "${local.prefix}-migrate"
  display_name = "LLM Gateway ${var.environment} schema migration"
}

resource "google_service_account" "role_config" {
  project      = var.project_id
  account_id   = "${local.prefix}-role-config"
  display_name = "LLM Gateway ${var.environment} database role configuration"
}

resource "google_service_account" "gateway" {
  project      = var.project_id
  account_id   = "${local.prefix}-gateway"
  display_name = "LLM Gateway ${var.environment} inference runtime"
}

resource "google_service_account" "control" {
  project      = var.project_id
  account_id   = "${local.prefix}-control"
  display_name = "LLM Gateway ${var.environment} control plane"
}

resource "google_service_account" "metering" {
  project      = var.project_id
  account_id   = "${local.prefix}-metering"
  display_name = "LLM Gateway ${var.environment} Metering"
}

resource "google_service_account" "bff" {
  project      = var.project_id
  account_id   = "${local.prefix}-bff"
  display_name = "LLM Gateway ${var.environment} WorkOS console BFF"
}

resource "google_secret_manager_secret_iam_member" "bff_gateway_api_key" {
  project   = var.project_id
  secret_id = data.google_secret_manager_secret.bff_gateway_api_key.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.bff.email}"
}

resource "google_project_iam_member" "cloud_sql_client" {
  for_each = local.cloud_sql_accounts
  project  = var.project_id
  role     = "roles/cloudsql.client"
  member   = "serviceAccount:${each.value}"
}

resource "google_project_iam_member" "build_log_writer" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.build.email}"
}

resource "google_artifact_registry_repository_iam_member" "build_writer" {
  project    = var.project_id
  location   = google_artifact_registry_repository.gateway.location
  repository = google_artifact_registry_repository.gateway.name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.build.email}"
}

resource "google_storage_bucket_iam_member" "build_source" {
  bucket = google_storage_bucket.build_source.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.build.email}"
}

resource "google_secret_manager_secret_iam_member" "database_access" {
  for_each  = local.database_secret_access
  project   = var.project_id
  secret_id = each.value[0]
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${each.value[1]}"
}

resource "google_secret_manager_secret_iam_member" "runtime_access" {
  for_each  = local.runtime_secret_access
  project   = var.project_id
  secret_id = each.value[0]
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${each.value[1]}"
}

resource "google_project_iam_custom_role" "provider_secret_custody" {
  project     = var.project_id
  role_id     = "llmGatewayProviderSecretCustody"
  title       = "LLM Gateway Provider Secret Custody"
  description = "Create and access immutable Provider Connection secret versions."
  permissions = [
    "secretmanager.secrets.create",
    "secretmanager.secrets.get",
    "secretmanager.versions.add",
    "secretmanager.versions.access",
    "secretmanager.versions.get",
  ]
}

resource "google_project_iam_member" "provider_secret_custody" {
  project = var.project_id
  role    = google_project_iam_custom_role.provider_secret_custody.name
  member  = "serviceAccount:${google_service_account.control.email}"
}

resource "google_project_iam_custom_role" "metering_export" {
  project     = var.project_id
  role_id     = "llmGatewayMeteringExport"
  title       = "LLM Gateway Metering Export"
  description = "Read bucket metadata and create/read immutable Metering exports."
  permissions = [
    "storage.buckets.get",
    "storage.objects.create",
    "storage.objects.get",
  ]
}

resource "google_storage_bucket_iam_member" "metering_export" {
  bucket = google_storage_bucket.metering_exports.name
  role   = google_project_iam_custom_role.metering_export.name
  member = "serviceAccount:${google_service_account.metering.email}"
}
