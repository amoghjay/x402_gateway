# Workload Identity Federation: GitHub Actions pushes images without a stored key.
#
# How a push authenticates, end to end:
#   1. GitHub mints a short-lived OIDC token for the workflow run. Its claims say which
#      repository, branch and workflow the run belongs to.
#   2. google-github-actions/auth sends that token to Google's STS. The pool's provider
#      below checks the issuer and the attribute_condition, and returns a federated token.
#   3. The federated identity impersonates the CI service account (roles/iam.workloadIdentityUser),
#      getting an access token valid for about an hour.
#   4. `docker push` uses that token, and the service account may only write to the one
#      repository in registry.tf.
#
# Nothing long-lived exists anywhere: no JSON key in GitHub secrets to leak or rotate.

# A pool groups external identities. One pool for GitHub; other CI systems would get their own.
resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"
  description               = "OIDC identities from GitHub Actions workflow runs"
  depends_on                = [google_project_service.apis]
}

# A provider says which tokens the pool trusts and how their claims map to attributes.
resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "x402-gateway"
  display_name                       = "x402_gateway repository"

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }

  # Claims from GitHub's token become attributes we can match on in IAM bindings.
  attribute_mapping = {
    "google.subject"       = "assertion.sub" # e.g. repo:amoghjay/x402_gateway:ref:refs/heads/main
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"
  }

  # The gate. GitHub's issuer signs tokens for every repository on github.com, so without
  # this condition any public repo could federate into this pool. It matches on the
  # numeric repository and owner IDs, not the name: if the repo were renamed or deleted,
  # someone re-creating "amoghjay/x402_gateway" would get new IDs and no access.
  # Only runs on main may push; pull requests (including from forks) build but never push.
  attribute_condition = <<-EOT
    assertion.repository_id == "${var.github_repository_id}" &&
    assertion.repository_owner_id == "${var.github_owner_id}" &&
    assertion.ref == "refs/heads/main"
  EOT
}

# The identity CI acts as once federated. It holds one permission: write to our repository.
resource "google_service_account" "ci" {
  account_id   = "x402-ci-push"
  display_name = "x402 CI image push (GitHub Actions via WIF)"
  depends_on   = [google_project_service.apis]
}

resource "google_artifact_registry_repository_iam_member" "ci_push" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.ci.email}"
}

# Who may impersonate the CI service account: any federated identity from this repository.
# The provider's attribute_condition has already narrowed that to runs on main.
resource "google_service_account_iam_member" "ci_wif" {
  service_account_id = google_service_account.ci.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repository}"
}
