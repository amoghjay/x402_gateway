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
