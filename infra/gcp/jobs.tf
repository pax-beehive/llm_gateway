resource "google_cloud_run_v2_job" "schema_migrate" {
  project             = var.project_id
  location            = var.region
  name                = "${local.prefix}-schema-migrate"
  labels              = merge(local.labels, { component = "schema-migrate" })
  deletion_protection = true

  template {
    task_count  = 1
    parallelism = 1

    template {
      service_account       = google_service_account.migration.email
      execution_environment = "EXECUTION_ENVIRONMENT_GEN2"
      timeout               = "1200s"
      max_retries           = 0

      containers {
        image = var.schema_migrate_image

        env {
          name  = "SCHEMA_MIGRATION_CONFIRM"
          value = "apply"
        }
        env {
          name  = "SCHEMA_MIGRATION_CLOUD_SQL_INSTANCE"
          value = google_sql_database_instance.gateway.connection_name
        }
        env {
          name = "ADMIN_DATABASE_URL"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.admin_database.secret_id
              version = google_secret_manager_secret_version.admin_database.version
            }
          }
        }
      }
    }
  }

  depends_on = [
    google_project_service.required,
    google_sql_database.gateway,
    google_sql_user.database,
    google_secret_manager_secret_iam_member.database_access,
  ]
}

resource "google_cloud_run_v2_job" "role_config" {
  project             = var.project_id
  location            = var.region
  name                = "${local.prefix}-role-config"
  labels              = merge(local.labels, { component = "role-config" })
  deletion_protection = true

  template {
    task_count  = 1
    parallelism = 1

    template {
      service_account       = google_service_account.role_config.email
      execution_environment = "EXECUTION_ENVIRONMENT_GEN2"
      timeout               = "1200s"
      max_retries           = 0

      containers {
        image = var.role_config_image

        env {
          name  = "ROLE_CONFIGURATION_CONFIRM"
          value = "apply"
        }
        env {
          name  = "TENANT_ADMIN_DB_ROLE"
          value = "llm_gateway_control"
        }
        env {
          name  = "GATEWAY_DB_ROLE"
          value = "llm_gateway_runtime"
        }
        env {
          name  = "METERING_DB_ROLE"
          value = "llm_gateway_metering"
        }
        env {
          name = "ADMIN_DATABASE_URL"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.admin_database.secret_id
              version = google_secret_manager_secret_version.admin_database.version
            }
          }
        }

        volume_mounts {
          name       = "cloudsql"
          mount_path = "/cloudsql"
        }
      }

      volumes {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [google_sql_database_instance.gateway.connection_name]
        }
      }
    }
  }

  depends_on = [
    google_project_service.required,
    google_sql_database.gateway,
    google_sql_user.database,
    google_secret_manager_secret_iam_member.database_access,
  ]
}
