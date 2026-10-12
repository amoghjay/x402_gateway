data "google_project" "this" {}

resource "google_billing_budget" "monthly" {
  billing_account = var.billing_account
  display_name    = "x402-envoy monthly"

  budget_filter {
    projects = ["projects/${data.google_project.this.number}"]

    # Measure gross spend. On the free trial every charge is covered by credits, so a
    # budget that counted them would read $0 forever and never alert.
    credit_types_treatment = "EXCLUDE_ALL_CREDITS"
  }

  amount {
    specified_amount {
      currency_code = "USD" # must match the billing account's currency
      units         = tostring(var.budget_usd)
    }
  }

  # Alerts go to the billing account's admins by email (the default recipients).
  threshold_rules {
    threshold_percent = 0.25
  }
  threshold_rules {
    threshold_percent = 0.5
  }
  threshold_rules {
    threshold_percent = 0.9
  }
  threshold_rules {
    threshold_percent = 1.0
    spend_basis       = "FORECASTED_SPEND" # warns before the month ends, not after
  }

  depends_on = [google_project_service.apis]
}
