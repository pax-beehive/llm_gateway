resource "google_compute_network" "runtime" {
  project                 = var.project_id
  name                    = "${local.prefix}-runtime"
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"

  lifecycle {
    prevent_destroy = true
  }

  depends_on = [google_project_service.required]
}

resource "google_compute_subnetwork" "runtime" {
  project                  = var.project_id
  name                     = "${local.prefix}-runtime-${var.region}"
  region                   = var.region
  network                  = google_compute_network.runtime.id
  ip_cidr_range            = "10.90.0.0/24"
  private_ip_google_access = true

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_compute_router" "runtime" {
  project = var.project_id
  name    = "${local.prefix}-runtime-${var.region}"
  region  = var.region
  network = google_compute_network.runtime.id

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_compute_router_nat" "runtime" {
  project                            = var.project_id
  name                               = "${local.prefix}-runtime-${var.region}"
  region                             = var.region
  router                             = google_compute_router.runtime.name
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"

  subnetwork {
    name                    = google_compute_subnetwork.runtime.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }

  lifecycle {
    prevent_destroy = true
  }
}
