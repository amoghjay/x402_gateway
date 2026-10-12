provider "google" {
  project = var.project_id
  region  = var.region

  # The Billing Budgets API needs a quota project for user credentials (ADC). Without
  # these two lines the budget create fails with a "quota project not set" error.
  user_project_override = true
  billing_project       = var.project_id
}
