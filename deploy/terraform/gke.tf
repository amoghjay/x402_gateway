resource "google_container_cluster" "x402" {
  name     = var.cluster_name
  location = var.zone # a zone, not a region: zonal control plane, covered by the free tier

  network         = google_compute_network.vpc.id
  subnetwork      = google_compute_subnetwork.gke.id
  networking_mode = "VPC_NATIVE"
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  # Node pools are managed as separate resources below. GKE insists on creating a
  # cluster with one pool, so it gets a one-node default pool that is deleted at once.
  remove_default_node_pool = true
  initial_node_count       = 1

  release_channel {
    channel = "REGULAR"
  }

  # Nodes get no external IPs and egress through Cloud NAT. The control plane endpoint
  # stays public so kubectl works from a laptop; access to it is still IAM-checked.
  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
  }

  # Pods authenticate to Google APIs as Kubernetes service accounts mapped to IAM,
  # never with exported keys and never as the node.
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # Dataplane V2 (eBPF, Cilium-based): enforces NetworkPolicy natively, which Phase 3
  # relies on for isolating the filter, Redis and the settler.
  datapath_provider = "ADVANCED_DATAPATH"

  # Envoy Gateway installs and owns the Gateway API CRDs. GKE's managed copy would be a
  # second owner of the same CRDs at a possibly different version.
  gateway_api_config {
    channel = "CHANNEL_DISABLED"
  }

  # One Prometheus in this cluster: kube-prometheus-stack (Step 5). GKE's system
  # metrics still go to Cloud Monitoring.
  monitoring_config {
    enable_components = ["SYSTEM_COMPONENTS"]
    managed_prometheus {
      enabled = false
    }
  }

  # Scale down empty nodes aggressively: spot capacity we are not using is money.
  cluster_autoscaling {
    autoscaling_profile = "OPTIMIZE_UTILIZATION"
  }

  # `terraform destroy` will refuse while this is true. Pausing the cluster is done by
  # scaling node pools to zero, never by deleting it.
  deletion_protection = true

  depends_on = [google_project_service.apis]
}

locals {
  # Shared by both pools. 40GB pd-balanced: the default 100GB would exhaust the
  # trial's 250GB SSD quota at three nodes.
  node_defaults = {
    disk_type    = "pd-balanced"
    disk_size_gb = 40
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"] # IAM decides, not scopes
  }
}

# On-demand, untainted: everything lands here unless it explicitly opts into spot.
# Platform components (Envoy Gateway, Redis, Prometheus, controllers) live here so a
# spot preemption never takes out the thing that recovers from it.
resource "google_container_node_pool" "system" {
  name     = "system"
  cluster  = google_container_cluster.x402.id
  location = var.zone

  initial_node_count = 1
  autoscaling {
    min_node_count = 1
    max_node_count = 2
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  # During an upgrade GKE adds one fresh node before draining an old one. Peak CPU is
  # (2 + 3 nodes) x 2 vCPU + 2 surge = 12: exactly the trial's CPUS_ALL_REGIONS.
  upgrade_settings {
    max_surge       = 1
    max_unavailable = 0
  }

  node_config {
    machine_type    = "e2-standard-2"
    disk_type       = local.node_defaults.disk_type
    disk_size_gb    = local.node_defaults.disk_size_gb
    oauth_scopes    = local.node_defaults.oauth_scopes
    service_account = google_service_account.nodes.email
    labels          = { pool = "system" }

    workload_metadata_config {
      mode = "GKE_METADATA" # the Workload Identity metadata server, not the node's own
    }
    shielded_instance_config {
      enable_secure_boot = true
    }
  }
}

# Spot: up to ~60-90% cheaper, and GCP may reclaim a node at any time with ~30s notice.
# Tainted, so only pods that tolerate preemption are scheduled here.
resource "google_container_node_pool" "workload" {
  name     = "workload"
  cluster  = google_container_cluster.x402.id
  location = var.zone

  initial_node_count = 1
  autoscaling {
    min_node_count = 0
    max_node_count = 3
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  # No surge here: replacing in place keeps the CPU peak inside the quota.
  upgrade_settings {
    max_surge       = 0
    max_unavailable = 1
  }

  node_config {
    machine_type    = "e2-standard-2"
    spot            = true
    disk_type       = local.node_defaults.disk_type
    disk_size_gb    = local.node_defaults.disk_size_gb
    oauth_scopes    = local.node_defaults.oauth_scopes
    service_account = google_service_account.nodes.email
    labels          = { pool = "workload" }

    taint {
      key    = "pool"
      value  = "workload"
      effect = "NO_SCHEDULE"
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    shielded_instance_config {
      enable_secure_boot = true
    }
  }

  lifecycle {
    # The autoscaler owns the live node count; Terraform only set the starting point.
    ignore_changes = [initial_node_count]
  }
}
