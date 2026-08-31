locals {
  control_service_name  = "${local.prefix}-control-plane"
  gateway_service_name  = "${local.prefix}-gateway"
  metering_service_name = "${local.prefix}-metering"
}

resource "google_cloud_run_v2_service" "control_plane" {
  project             = var.project_id
  location            = var.region
  name                = local.control_service_name
  labels              = merge(local.labels, { component = "control-plane" })
  ingress             = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
  deletion_protection = true

  template {
    service_account                  = google_service_account.control.email
    execution_environment            = "EXECUTION_ENVIRONMENT_GEN2"
    timeout                          = "30s"
    max_instance_request_concurrency = 40

    scaling {
      min_instance_count = 1
      max_instance_count = 10
    }

    vpc_access {
      egress = "ALL_TRAFFIC"
      network_interfaces {
        network    = google_compute_network.runtime.name
        subnetwork = google_compute_subnetwork.runtime.name
        tags       = ["llm-gateway-control"]
      }
    }

    containers {
      image = var.control_plane_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle          = false
        startup_cpu_boost = true
      }

      startup_probe {
        initial_delay_seconds = 0
        timeout_seconds       = 2
        period_seconds        = 5
        failure_threshold     = 48
        http_get {
          path = "/readyz"
          port = 8080
        }
      }

      liveness_probe {
        initial_delay_seconds = 10
        timeout_seconds       = 2
        period_seconds        = 10
        failure_threshold     = 3
        http_get {
          path = "/healthz"
          port = 8080
        }
      }

      env {
        name  = "CONTROL_PLANE_ADDR"
        value = ":8080"
      }
      env {
        name  = "CONTROL_PLANE_DEV_MODE"
        value = "false"
      }
      env {
        name  = "CONTROL_PLANE_MIGRATE"
        value = "false"
      }
      env {
        name  = "CONTROL_PLANE_DATABASE_TRANSPORT_ATTESTATION"
        value = "authenticated-encrypted"
      }
      env {
        name  = "CONTROL_PLANE_CLOUD_SQL_INSTANCE"
        value = google_sql_database_instance.gateway.connection_name
      }
      env {
        name  = "CONTROL_PLANE_DB_ROLE"
        value = "llm_gateway_control"
      }
      env {
        name  = "CONTROL_IAM_DENY_ALL"
        value = "true"
      }
      env {
        name  = "CONTROL_API_KEY_CURRENT_DIGEST_VERSION"
        value = "1"
      }
      env {
        name  = "CONTROL_SECRET_CUSTODY_BACKEND"
        value = "gcp-secret-manager"
      }
      env {
        name  = "CONTROL_GCP_SECRET_PROJECT"
        value = var.project_id
      }
      env {
        name  = "CONTROL_PROVIDER_LIVE_OPERATIONS"
        value = "disabled"
      }
      env {
        name  = "CONTROL_GATEWAY_REGIONS_JSON"
        value = jsonencode({ "gateway-us-west" = "us-west" })
      }
      env {
        name  = "CONTROL_METERING_REGIONS_JSON"
        value = jsonencode({ "metering-us-west1" = "us-west1" })
      }
      env {
        name = "CONTROL_PLANE_DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.control_database.secret_id
            version = google_secret_manager_secret_version.control_database.version
          }
        }
      }
      env {
        name = "CONTROL_API_KEY_PEPPERS_JSON"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.api_key_peppers.secret_id
            version = google_secret_manager_secret_version.api_key_peppers.version
          }
        }
      }
      env {
        name = "CONTROL_GATEWAY_HMAC_KEYS_JSON"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.control_gateway_hmac_keys.secret_id
            version = google_secret_manager_secret_version.control_gateway_hmac_keys.version
          }
        }
      }
      env {
        name = "CONTROL_METERING_HMAC_KEYS_JSON"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.control_metering_hmac_keys.secret_id
            version = google_secret_manager_secret_version.control_metering_hmac_keys.version
          }
        }
      }
    }
  }

  depends_on = [
    google_project_service.required,
    google_sql_database.gateway,
    google_secret_manager_secret_iam_member.database_access,
    google_secret_manager_secret_iam_member.runtime_access,
  ]

  lifecycle {
    ignore_changes = [
      client,
      client_version,
      template[0].containers[0].image,
    ]
  }
}

resource "google_cloud_run_v2_service" "metering" {
  project             = var.project_id
  location            = var.region
  name                = local.metering_service_name
  labels              = merge(local.labels, { component = "metering" })
  ingress             = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
  deletion_protection = true

  template {
    service_account                  = google_service_account.metering.email
    execution_environment            = "EXECUTION_ENVIRONMENT_GEN2"
    timeout                          = "30s"
    max_instance_request_concurrency = 20

    scaling {
      min_instance_count = 1
      max_instance_count = 5
    }

    vpc_access {
      egress = "ALL_TRAFFIC"
      network_interfaces {
        network    = google_compute_network.runtime.name
        subnetwork = google_compute_subnetwork.runtime.name
        tags       = ["llm-gateway-metering"]
      }
    }

    containers {
      image = var.metering_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle          = false
        startup_cpu_boost = true
      }

      startup_probe {
        initial_delay_seconds = 0
        timeout_seconds       = 2
        period_seconds        = 5
        failure_threshold     = 48
        http_get {
          path = "/readyz"
          port = 8080
        }
      }

      liveness_probe {
        initial_delay_seconds = 10
        timeout_seconds       = 2
        period_seconds        = 10
        failure_threshold     = 3
        http_get {
          path = "/healthz"
          port = 8080
        }
      }

      env {
        name  = "METERING_ADDR"
        value = ":8080"
      }
      env {
        name  = "METERING_DEV_MODE"
        value = "false"
      }
      env {
        name  = "METERING_MIGRATE"
        value = "false"
      }
      env {
        name  = "METERING_DATABASE_TRANSPORT_ATTESTATION"
        value = "authenticated-encrypted"
      }
      env {
        name  = "METERING_CLOUD_SQL_INSTANCE"
        value = google_sql_database_instance.gateway.connection_name
      }
      env {
        name  = "METERING_DB_ROLE"
        value = "llm_gateway_metering"
      }
      env {
        name  = "METERING_IAM_DENY_ALL"
        value = "true"
      }
      env {
        name  = "METERING_EXPORT_BACKEND"
        value = "gcs"
      }
      env {
        name  = "METERING_EXPORT_GCS_BUCKET"
        value = google_storage_bucket.metering_exports.name
      }
      env {
        name  = "METERING_EXPORT_GCS_PREFIX"
        value = "exports"
      }
      env {
        name  = "METERING_EXPORT_GCS_REGION"
        value = var.region
      }
      env {
        name  = "METERING_OPERATIONS_URL"
        value = google_cloud_run_v2_service.control_plane.uri
      }
      env {
        name  = "METERING_CLOUD_RUN_AUDIENCE"
        value = google_cloud_run_v2_service.control_plane.uri
      }
      env {
        name  = "METERING_ID"
        value = "metering-us-west1"
      }
      env {
        name  = "METERING_REGION"
        value = "us-west1"
      }
      env {
        name  = "METERING_WORKER_ID"
        value = "metering-us-west1"
      }
      env {
        name = "METERING_DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.metering_database.secret_id
            version = google_secret_manager_secret_version.metering_database.version
          }
        }
      }
      env {
        name = "METERING_OPERATIONS_HMAC_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.metering_operations_hmac.secret_id
            version = google_secret_manager_secret_version.metering_operations_hmac.version
          }
        }
      }
      env {
        name = "METERING_EXPORT_SIGNING_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.metering_export_signing.secret_id
            version = google_secret_manager_secret_version.metering_export_signing.version
          }
        }
      }
    }
  }

  depends_on = [
    google_cloud_run_v2_service_iam_member.control_invoker,
    google_project_service.required,
    google_sql_database.gateway,
    google_secret_manager_secret_iam_member.database_access,
    google_secret_manager_secret_iam_member.runtime_access,
  ]

  lifecycle {
    ignore_changes = [
      client,
      client_version,
      template[0].containers[0].image,
    ]
  }
}

resource "google_cloud_run_v2_service" "gateway" {
  count = var.gateway_service_enabled ? 1 : 0

  project             = var.project_id
  location            = var.region
  name                = local.gateway_service_name
  labels              = merge(local.labels, { component = "gateway" })
  ingress             = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
  deletion_protection = true

  template {
    service_account                  = google_service_account.gateway.email
    execution_environment            = "EXECUTION_ENVIRONMENT_GEN2"
    timeout                          = "300s"
    max_instance_request_concurrency = 40

    scaling {
      min_instance_count = 3
      max_instance_count = 30
    }

    vpc_access {
      egress = "ALL_TRAFFIC"
      network_interfaces {
        network    = google_compute_network.runtime.name
        subnetwork = google_compute_subnetwork.runtime.name
        tags       = ["llm-gateway-data-plane"]
      }
    }

    containers {
      image = var.gateway_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "1Gi"
        }
        cpu_idle          = false
        startup_cpu_boost = true
      }

      startup_probe {
        initial_delay_seconds = 0
        timeout_seconds       = 2
        period_seconds        = 5
        failure_threshold     = 48
        http_get {
          path = "/readyz"
          port = 8080
        }
      }

      liveness_probe {
        initial_delay_seconds = 10
        timeout_seconds       = 2
        period_seconds        = 10
        failure_threshold     = 3
        http_get {
          path = "/healthz"
          port = 8080
        }
      }

      env {
        name  = "GATEWAY_ADDR"
        value = ":8080"
      }
      env {
        name  = "GATEWAY_ENV"
        value = "production"
      }
      env {
        name  = "GATEWAY_MIGRATE"
        value = "false"
      }
      env {
        name  = "GATEWAY_DURABILITY_ATTESTATION"
        value = "sync-multi-az"
      }
      env {
        name  = "GATEWAY_DATABASE_TRANSPORT_ATTESTATION"
        value = "authenticated-encrypted"
      }
      env {
        name  = "GATEWAY_CLOUD_SQL_INSTANCE"
        value = google_sql_database_instance.gateway.connection_name
      }
      env {
        name  = "GATEWAY_LOCAL_REGION"
        value = "us-west"
      }
      env {
        name  = "GATEWAY_ID"
        value = "gateway-us-west"
      }
      env {
        name  = "GATEWAY_CACHE_PROTECTION_MODE"
        value = "off"
      }
      env {
        name  = "GATEWAY_ACCESS_PROJECTION"
        value = "true"
      }
      env {
        name  = "GATEWAY_ROUTING_CATALOG"
        value = "true"
      }
      env {
        name  = "GATEWAY_API_KEY_CURRENT_DIGEST_VERSION"
        value = "1"
      }
      env {
        name  = "GATEWAY_CONTROL_RELAY_URL"
        value = google_cloud_run_v2_service.control_plane.uri
      }
      env {
        name  = "GATEWAY_OPERATIONS_URL"
        value = google_cloud_run_v2_service.control_plane.uri
      }
      env {
        name  = "GATEWAY_CLOUD_RUN_AUDIENCE"
        value = google_cloud_run_v2_service.control_plane.uri
      }
      env {
        name = "DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.gateway_database.secret_id
            version = google_secret_manager_secret_version.gateway_database.version
          }
        }
      }
      env {
        name = "GATEWAY_API_KEY_PEPPERS_JSON"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.api_key_peppers.secret_id
            version = google_secret_manager_secret_version.api_key_peppers.version
          }
        }
      }
      env {
        name = "GATEWAY_CONTROL_RELAY_HMAC_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.gateway_control_hmac.secret_id
            version = google_secret_manager_secret_version.gateway_control_hmac.version
          }
        }
      }
      env {
        name = "GATEWAY_OPERATIONS_HMAC_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.gateway_control_hmac.secret_id
            version = google_secret_manager_secret_version.gateway_control_hmac.version
          }
        }
      }
    }
  }

  depends_on = [
    google_cloud_run_v2_service_iam_member.control_invoker,
    google_project_service.required,
    google_sql_database.gateway,
    google_secret_manager_secret_iam_member.database_access,
    google_secret_manager_secret_iam_member.runtime_access,
  ]

  lifecycle {
    ignore_changes = [
      client,
      client_version,
      template[0].containers[0].image,
    ]
  }
}

resource "google_cloud_run_v2_service_iam_member" "control_invoker" {
  for_each = {
    gateway  = google_service_account.gateway.email
    metering = google_service_account.metering.email
  }

  project  = var.project_id
  location = google_cloud_run_v2_service.control_plane.location
  name     = google_cloud_run_v2_service.control_plane.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${each.value}"
}
