# Every API the stack needs, enabled here so a fresh project is one apply away from
# ready. Later phases add to this list rather than enabling APIs by hand.
locals {
  apis = [
    "cloudresourcemanager", # reading project metadata (data.google_project)
    "serviceusage",         # enabling the rest of this list
    "billingbudgets",       # budget.tf
    "compute",              # VPC, static IP, GKE nodes
    "container",            # GKE
    "artifactregistry",     # container images
    "iam",                  # service accounts
    "iamcredentials",       # Workload Identity: short-lived tokens
    "sts",                  # Workload Identity Federation for GitHub Actions
  ]
}

resource "google_project_service" "apis" {
  for_each = toset([for api in local.apis : "${api}.googleapis.com"])
  service  = each.value

  # Destroying this stack must never switch an API off underneath resources that
  # Terraform does not manage, such as the state bucket.
  disable_on_destroy = false
}
