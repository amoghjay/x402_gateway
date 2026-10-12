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

## Sizing against the free-trial quotas

`CPUS_ALL_REGIONS` is 12 and cannot be raised on the trial: `system` (max 2) + `workload`
(max 3) x 2 vCPU + one 2-vCPU upgrade surge = 12. `SSD_TOTAL_GB` is 250: hence 40GB boot
disks instead of the 100GB default. Private nodes keep `IN_USE_ADDRESSES` (8) free.

## Files

| File | What |
|---|---|
| `versions.tf` | Terraform and provider pins; the (empty) GCS backend |
| `providers.tf` | google provider; quota project for the Budgets API |
| `variables.tf` | inputs; `project_id` refuses `radius-*` work projects |
| `apis.tf` | every API the stack uses |
| `budget.tf` | monthly budget on gross spend (credits excluded), alerts at 25/50/90% and forecast 100% |
| `network.tf` | dedicated VPC; subnet with secondary ranges for pods and Services; Cloud NAT for private nodes; the reserved Gateway IP |
| `iam.tf` | least-privilege node service account (not the Editor-holding default) |
| `gke.tf` | zonal cluster (private nodes, Workload Identity, Dataplane V2, deletion protection) and two pools: on-demand `system`, tainted spot `workload`; `paused` scales both to zero (`make gke-down` / `make gke-up`) |
| `.terraform.lock.hcl` | provider checksums for darwin_arm64 and linux_amd64; committed |
