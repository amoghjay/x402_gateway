# deploy/k8s/demo-platform

A route whose rules come from a Radius Platform project instead of a fixed price: the
controller polls the project's compiled policy and the filter enforces it by method and
path (free, denied, rate limited, a free quota, or paid in any of the project's options).

It is kept out of `demo/` because it needs two things that are yours, not the repo's: a
project, and a key to read it.

## 1. The key, as a labelled Secret

Create a connector key on the project's Gate page in the Radius Platform console. Store
it in the controller's namespace. `read -s` keeps it out of your shell history:

```bash
read -s KEY && kubectl -n x402-system create secret generic radius-platform \
  --from-literal=apiKey="$KEY"; unset KEY
kubectl -n x402-system label secret radius-platform x402.dev/platform-credentials=true
```

The controller reads only Secrets with that label, so a `PaymentPolicy` cannot name some
other Secret and have it sent to the Platform.

## 2. The route and its policy

```bash
ORG=org_...            # your organization ID
PROJECT=00000000-...   # the project's ID
sed -e "s/REPLACE_ORG_ID/$ORG/" -e "s/00000000-0000-0000-0000-000000000000/$PROJECT/" \
  platform.yaml | kubectl apply -f -
kubectl -n demo get pp httpbin-platform -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

`Synced=True` means the controller has fetched the project's compiled policy. Until the
first fetch succeeds the route answers 503 `policy_unavailable` (`noPolicy: Deny`), never
free. After that, a failed fetch keeps the last good policy in force.

## 3. Endpoints the project needs

The route sends `/anything/platform` to httpbin, so the project's endpoints should be
paths under it, for example `/anything/platform/paid/:id` with a charge rule and
`/anything/platform/free` with an allow rule. A path the project has no rule for follows
the policy's default.
