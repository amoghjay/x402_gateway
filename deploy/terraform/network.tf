# A dedicated VPC rather than the project's "default" network: explicit ranges, no
# auto-created subnets in every region, nothing we did not declare.
resource "google_compute_network" "vpc" {
  name                    = "x402"
  auto_create_subnetworks = false
  depends_on              = [google_project_service.apis]
}

# VPC-native GKE: nodes get IPs from the primary range, and pods and Services get
# their own *secondary* ranges on the same subnet. Pod IPs are then real VPC IPs:
# routable, visible to firewall rules, and no overlay network in between.
resource "google_compute_subnetwork" "gke" {
  name          = "x402-gke"
  region        = var.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.10.0.0/20" # nodes: 4k addresses, far more than 12 CPUs will ever need

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.20.0.0/16" # GKE carves a /24 per node (110 pods), so a /16 is ~256 nodes
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.30.0.0/20" # ClusterIPs
  }

  # Private nodes have no external IP; this lets them still reach Google APIs
  # (Artifact Registry, Cloud Logging) over Google's network.
  private_ip_google_access = true
}

# Private nodes need a way out for everything else: Docker Hub pulls, the Radius
# facilitator, the Radius RPC. Cloud NAT gives them egress without inbound exposure.
resource "google_compute_router" "nat" {
  name    = "x402-nat"
  region  = var.region
  network = google_compute_network.vpc.id
}

resource "google_compute_router_nat" "nat" {
  name                               = "x402-nat"
  router                             = google_compute_router.nat.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  log_config {
    enable = true
    filter = "ERRORS_ONLY" # dropped connections show up in Cloud Logging
  }
}

# The public IP the Envoy Gateway LoadBalancer will claim (Step 5). Reserved now so it
# survives the Gateway being deleted and recreated, and so a TLS hostname can point at
# it before the Gateway exists. Costs ~$7/month while not attached to anything.
resource "google_compute_address" "gateway" {
  name         = "x402-gateway"
  region       = var.region
  address_type = "EXTERNAL"
  depends_on   = [google_project_service.apis]
}
