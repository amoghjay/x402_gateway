# deploy/terraform

GCP foundation for the x402 stack: project APIs and a budget today; VPC, GKE,
Artifact Registry, Workload Identity Federation and a static IP as Phase 1 continues.

## Prerequisites

- Run from inside the repo so direnv loads `.envrc`: the personal gcloud identity,
  its ADC, and a separate `KUBECONFIG`. `gcloud config list` must show the personal
  account before any command below.
- Bootstrap, once per project, by hand. Each item is something Terraform needs before
  it can run, so Terraform cannot create it:
  - the state bucket: Terraform cannot create the bucket it stores its own state in;
  - `gcloud services enable cloudresourcemanager.googleapis.com`: `data.google_project`
    is read during `plan`, before any `google_project_service` has been applied.
    (`serviceusage` is already on in new projects.) `apis.tf` still lists both, so
    Terraform adopts them on the first apply.

## First run

```bash
cp backend.hcl.example backend.hcl            # fill in; gitignored
cp terraform.tfvars.example terraform.tfvars  # fill in; gitignored
terraform init -backend-config=backend.hcl
terraform plan -out=tfplan
terraform apply tfplan
```

On a fresh project the first apply can fail with a 403 on a resource whose API the same
apply just enabled: enabling is eventually consistent and takes a minute or two to
propagate. Nothing rolls back; re-plan and apply, and only the failed resources remain.

## Files

| File | What |
|---|---|
| `versions.tf` | Terraform and provider pins; the (empty) GCS backend |
| `providers.tf` | google provider; quota project for the Budgets API |
| `variables.tf` | inputs; `project_id` refuses `radius-*` work projects |
| `apis.tf` | every API the stack uses |
| `budget.tf` | monthly budget on gross spend (credits excluded), alerts at 25/50/90% and forecast 100% |
| `.terraform.lock.hcl` | provider checksums for darwin_arm64 and linux_amd64; committed |
