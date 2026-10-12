terraform {
  required_version = ">= 1.13"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.6"
    }
  }

  # State lives in the versioned GCS bucket created by hand in Phase 1 step 2a: the
  # bucket has to exist before Terraform can store anything in it. A backend block
  # cannot read variables, so bucket and prefix come from backend.hcl at init time
  # (gitignored; see backend.hcl.example). The GCS backend locks state natively.
  backend "gcs" {}
}
