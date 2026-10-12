# Nodes run as a dedicated service account with only what a node needs. The default
# is the Compute Engine default SA, which holds Editor on the whole project: any pod
# that reached the node's metadata server would inherit it.
resource "google_service_account" "nodes" {
  account_id   = "x402-gke-nodes"
  display_name = "x402 GKE nodes"
  depends_on   = [google_project_service.apis]
}

resource "google_project_iam_member" "nodes" {
  for_each = toset([
    "roles/container.defaultNodeServiceAccount", # logging, monitoring, metadata: GKE's minimal node role
    "roles/artifactregistry.reader",             # pull our images
  ])
  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.nodes.email}"
}
