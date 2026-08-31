variable "project_id" {
  description = "GCP project that owns this isolated LLM Gateway deployment."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid GCP project id."
  }
}

variable "region" {
  description = "GCP region implementing the first Home Region."
  type        = string
  default     = "us-west1"
}

variable "environment" {
  description = "Deployment environment used in resource names and labels."
  type        = string
  default     = "prod"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,11}$", var.environment))
    error_message = "environment must be 2-12 lowercase letters, digits, or hyphens."
  }
}

variable "database_version" {
  description = "Cloud SQL PostgreSQL major selected after availability validation."
  type        = string
  default     = "POSTGRES_18"

  validation {
    condition     = contains(["POSTGRES_17", "POSTGRES_18"], var.database_version)
    error_message = "database_version must be POSTGRES_17 or POSTGRES_18."
  }
}

variable "database_tier" {
  description = "Initial HA production database tier; tune from measured load."
  type        = string
  default     = "db-custom-2-7680"
}

variable "backup_retained_count" {
  description = "Automated backups retained by Cloud SQL."
  type        = number
  default     = 14

  validation {
    condition     = var.backup_retained_count >= 7 && var.backup_retained_count <= 30
    error_message = "backup_retained_count must be between 7 and 30."
  }
}

variable "pitr_log_days" {
  description = "Days of transaction logs retained for PITR."
  type        = number
  default     = 7

  validation {
    condition     = var.pitr_log_days >= 1 && var.pitr_log_days <= 7
    error_message = "pitr_log_days must be between 1 and 7 for Cloud SQL Enterprise."
  }
}

variable "schema_migrate_image" {
  description = "Immutable schema-migrate image reference, including an Artifact Registry sha256 digest."
  type        = string

  validation {
    condition     = can(regex("^[^[:space:]]+@sha256:[0-9a-f]{64}$", var.schema_migrate_image))
    error_message = "schema_migrate_image must be an immutable image reference ending in @sha256:<64 lowercase hex characters>."
  }
}

variable "role_config_image" {
  description = "Immutable role-config image reference, including an Artifact Registry sha256 digest."
  type        = string

  validation {
    condition     = can(regex("^[^[:space:]]+@sha256:[0-9a-f]{64}$", var.role_config_image))
    error_message = "role_config_image must be an immutable image reference ending in @sha256:<64 lowercase hex characters>."
  }
}

variable "gateway_image" {
  description = "Immutable Gateway service image reference, including an Artifact Registry sha256 digest."
  type        = string

  validation {
    condition     = can(regex("^[^[:space:]]+@sha256:[0-9a-f]{64}$", var.gateway_image))
    error_message = "gateway_image must be an immutable image reference ending in @sha256:<64 lowercase hex characters>."
  }
}

variable "control_plane_image" {
  description = "Immutable Control Plane service image reference, including an Artifact Registry sha256 digest."
  type        = string

  validation {
    condition     = can(regex("^[^[:space:]]+@sha256:[0-9a-f]{64}$", var.control_plane_image))
    error_message = "control_plane_image must be an immutable image reference ending in @sha256:<64 lowercase hex characters>."
  }
}

variable "metering_image" {
  description = "Immutable Metering service image reference, including an Artifact Registry sha256 digest."
  type        = string

  validation {
    condition     = can(regex("^[^[:space:]]+@sha256:[0-9a-f]{64}$", var.metering_image))
    error_message = "metering_image must be an immutable image reference ending in @sha256:<64 lowercase hex characters>."
  }
}

variable "gateway_service_enabled" {
  description = "Create the Gateway only after a Provider Connection, Routing Catalog, Tenant, and API key are published."
  type        = bool
  default     = false
}
