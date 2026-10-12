variable "project_id" {
  type        = string
  description = "The personal GCP project for the x402 stack. Never a Radius work project."

  # The work gcloud config on this machine targets radius-* projects. This refuses a
  # plan against one before any API call is made, whatever the shell environment says.
  validation {
    condition     = !startswith(var.project_id, "radius-")
    error_message = "Refusing to target a Radius work project from the personal x402 stack."
  }
}

variable "region" {
  type        = string
  description = "Region for every regional resource."
  default     = "us-central1"
}

variable "billing_account" {
  type        = string
  description = "Billing account ID (XXXXXX-XXXXXX-XXXXXX) that the budget watches."
}

variable "budget_usd" {
  type        = number
  description = "Monthly budget in USD, measured before credits. A budget alerts; it never stops spend."
  default     = 100
}

variable "zone" {
  type        = string
  description = "Zone for the cluster. A zonal cluster is the one GKE's free tier pays the management fee for."
  default     = "us-central1-a"
}

variable "cluster_name" {
  type    = string
  default = "x402"
}
