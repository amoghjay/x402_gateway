# deploy/k8s

Raw manifests for what runs in the cluster. Third-party components are installed with
Helm at pinned versions; our own resources are plain YAML, applied with `kubectl`.
Run everything from inside the repo so direnv selects the personal `KUBECONFIG`.

## Order

```bash
# 1. Envoy Gateway v1.9.2: its CRDs, the Gateway API CRDs, and the controller
helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.9.2 \
  -n envoy-gateway-system --create-namespace
kubectl wait --timeout=5m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available

# 2. Our GatewayClass, its EnvoyProxy settings, and the Gateway on the reserved IP
kubectl apply -f gateway/

# 3. The demo backend and its route
kubectl apply -f demo/
```

| Path | What |
|---|---|
| `gateway/` | `gateway` namespace; `EnvoyProxy` (LoadBalancer on the reserved IP, proxies on the system pool); `GatewayClass x402`; `Gateway x402` with one HTTP listener that accepts routes only from labelled namespaces |
| `demo/` | `demo` namespace (labelled for routes); httpbin on the spot pool; an `HTTPRoute` sending `/` to it |
