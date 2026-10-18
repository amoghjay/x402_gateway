# One Docker repository for every image the stack builds: x402-filter now, x402-settler
# and x402-controller later. Image paths look like
#   us-central1-docker.pkg.dev/<project>/x402/x402-filter:<version>
#
# Regional, in the cluster's region: nodes pull over Google's network inside us-central1,
# so pulls cost no egress. Nodes can already pull: iam.tf grants their service account
# roles/artifactregistry.reader.
resource "google_artifact_registry_repository" "images" {
  repository_id = "x402"
  location      = var.region
  format        = "DOCKER"
  description   = "x402 filter, settler and controller images"

  # Tags stay mutable while we iterate: `make images` tags from `git describe --dirty`,
  # so rebuilding the same commit re-pushes the same tag. Mainnet (Phase 7) deploys by
  # digest, which is immutable whatever this setting says.
  docker_config {
    immutable_tags = false
  }

  # Storage beyond the free 0.5 GB is billed per GB-month. Keep the 10 newest versions
  # of each image and delete anything else older than 7 days. KEEP wins over DELETE, so
  # a long pause never empties the repository.
  cleanup_policy_dry_run = false

  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = 10
    }
  }

  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      tag_state  = "ANY"
      older_than = "604800s" # 7 days
    }
  }

  depends_on = [google_project_service.apis]
}
