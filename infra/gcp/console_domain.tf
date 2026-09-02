locals {
  console_domain_enabled      = var.bff_service_enabled && var.console_domain != ""
  console_certificate_enabled = local.console_domain_enabled && var.console_certificate_enabled
}

resource "google_compute_global_address" "console" {
  count = local.console_domain_enabled ? 1 : 0

  project      = var.project_id
  name         = "${local.prefix}-console-ipv4"
  address_type = "EXTERNAL"
  ip_version   = "IPV4"

  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.required]
}

resource "google_compute_region_network_endpoint_group" "console" {
  count = local.console_domain_enabled ? 1 : 0

  project               = var.project_id
  region                = var.region
  name                  = "${local.prefix}-console-neg"
  network_endpoint_type = "SERVERLESS"

  cloud_run {
    service = google_cloud_run_v2_service.bff[0].name
  }

  depends_on = [google_project_service.required]
}

resource "google_compute_backend_service" "console" {
  count = local.console_domain_enabled ? 1 : 0

  project               = var.project_id
  name                  = "${local.prefix}-console-backend"
  protocol              = "HTTP"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  enable_cdn            = false

  backend {
    group = google_compute_region_network_endpoint_group.console[0].id
  }

  log_config {
    enable      = true
    sample_rate = 1.0
  }
}

resource "google_compute_url_map" "console" {
  count = local.console_domain_enabled ? 1 : 0

  project         = var.project_id
  name            = "${local.prefix}-console"
  default_service = google_compute_backend_service.console[0].id
}

resource "google_compute_url_map" "console_http_redirect" {
  count = local.console_domain_enabled ? 1 : 0

  project = var.project_id
  name    = "${local.prefix}-console-http-redirect"

  default_url_redirect {
    https_redirect         = true
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
    strip_query            = false
  }
}

resource "google_certificate_manager_dns_authorization" "console" {
  count = local.console_domain_enabled ? 1 : 0

  project     = var.project_id
  location    = "global"
  name        = "${local.prefix}-console-dns-auth"
  description = "DNS authorization for ${var.console_domain}."
  domain      = var.console_domain
  type        = "FIXED_RECORD"
  labels      = local.labels

  depends_on = [google_project_service.required]
}

resource "google_certificate_manager_certificate" "console" {
  count = local.console_certificate_enabled ? 1 : 0

  project     = var.project_id
  location    = "global"
  name        = "${local.prefix}-console-${var.console_certificate_generation}"
  description = "Google-managed certificate for ${var.console_domain}."
  labels      = local.labels

  managed {
    domains            = [var.console_domain]
    dns_authorizations = [google_certificate_manager_dns_authorization.console[0].id]
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_certificate_manager_certificate_map" "console" {
  count = local.console_certificate_enabled ? 1 : 0

  project     = var.project_id
  name        = "${local.prefix}-console"
  description = "Certificate map for the LLM Gateway production console."
  labels      = local.labels
}

resource "google_certificate_manager_certificate_map_entry" "console" {
  count = local.console_certificate_enabled ? 1 : 0

  project      = var.project_id
  name         = "${local.prefix}-console"
  map          = google_certificate_manager_certificate_map.console[0].name
  hostname     = var.console_domain
  certificates = [google_certificate_manager_certificate.console[0].id]
  labels       = local.labels
}

resource "google_compute_ssl_policy" "console" {
  count = local.console_domain_enabled ? 1 : 0

  project         = var.project_id
  name            = "${local.prefix}-console"
  profile         = "MODERN"
  min_tls_version = "TLS_1_2"
}

resource "google_compute_target_https_proxy" "console" {
  count = local.console_certificate_enabled ? 1 : 0

  project         = var.project_id
  name            = "${local.prefix}-console"
  url_map         = google_compute_url_map.console[0].id
  certificate_map = "//certificatemanager.googleapis.com/${google_certificate_manager_certificate_map.console[0].id}"
  ssl_policy      = google_compute_ssl_policy.console[0].id
}

resource "google_compute_target_http_proxy" "console" {
  count = local.console_domain_enabled ? 1 : 0

  project = var.project_id
  name    = "${local.prefix}-console-http-redirect"
  url_map = google_compute_url_map.console_http_redirect[0].id
}

resource "google_compute_global_forwarding_rule" "console_https" {
  count = local.console_certificate_enabled ? 1 : 0

  project               = var.project_id
  name                  = "${local.prefix}-console-https"
  ip_address            = google_compute_global_address.console[0].id
  ip_protocol           = "TCP"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  network_tier          = "PREMIUM"
  port_range            = "443"
  target                = google_compute_target_https_proxy.console[0].id
  labels                = local.labels
}

resource "google_compute_global_forwarding_rule" "console_http" {
  count = local.console_domain_enabled ? 1 : 0

  project               = var.project_id
  name                  = "${local.prefix}-console-http"
  ip_address            = google_compute_global_address.console[0].id
  ip_protocol           = "TCP"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  network_tier          = "PREMIUM"
  port_range            = "80"
  target                = google_compute_target_http_proxy.console[0].id
  labels                = local.labels
}
