output "project_number" {
  description = "Numeric project ID; GKE and Workload Identity resources refer to it."
  value       = data.google_project.this.number
}
