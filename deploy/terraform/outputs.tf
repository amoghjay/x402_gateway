output "project_number" {
  description = "Numeric project ID; GKE and Workload Identity resources refer to it."
  value       = data.google_project.this.number
}

output "cluster_name" {
  value = google_container_cluster.x402.name
}

output "gateway_ip" {
  description = "Reserved public IP for the Envoy Gateway LoadBalancer (Step 5)."
  value       = google_compute_address.gateway.address
}

output "get_credentials" {
  description = "Writes the cluster's context into $KUBECONFIG (the personal one, via direnv)."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.x402.name} --zone ${var.zone} --project ${var.project_id}"
}

output "image_registry" {
  description = "Prefix for image names; `make images REGISTRY=<this>`."
  value       = "${google_artifact_registry_repository.images.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "wif_provider" {
  description = "workload_identity_provider input for google-github-actions/auth."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "ci_service_account" {
  description = "service_account input for google-github-actions/auth."
  value       = google_service_account.ci.email
}
