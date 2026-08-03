# rhai-maas-tokenizer

A Go service that watches for tenant namespaces on an OpenShift cluster and provisions MaaS (Model as a Service) credentials into each tenant namespace.

## How it works

1. **Discovers tenant namespaces** — finds all Namespaces with the `tenant` label (e.g. `user-x4ddm-demo`).

2. **Obtains MaaS API key** — calls `MAAS_URL/maas-api/v1/api-keys` with a generated name and configured expiry using a ServiceAccount bearer token (`MAAS_TOKEN`), then `MAAS_URL/maas-api/v1/models` to discover available models.

3. **Creates secrets in tenant namespaces** — creates a `maas-secret` Secret directly in each tenant namespace containing the API key and model URLs.

4. **Labels tenant namespaces** — after provisioning, labels each namespace `rhai-tmm.dev/maas-auth: done` and annotates with `rhai-tmm.dev/maas-auth-until` (UTC RFC3339 expiry time computed from `--token-expiry`).

5. **Namespace watcher** — watches for namespaces with the `tenant` label and triggers immediate provisioning.

6. **Periodic reconciliation** — re-runs every 10 minutes (configurable). Re-provisions any tenant namespace whose tokens have expired (based on the `maas-auth-until` annotation) or are missing.

7. **Deployment rollout restart** — after provisioning a secret, restarts any Deployments in the tenant namespace that carry the label `rhai-tmm.dev/reloader: "true"`. This ensures pods pick up the refreshed credentials.

## Configuration

### Required environment variables

- `MAAS_URL` — MaaS API base URL (e.g. `https://maas.apps.example.com`)
- `MAAS_TOKEN` — OpenShift ServiceAccount bearer token for MaaS API authentication

### Command-line flags

- `--token-expiry` — MaaS token expiration duration (default `8h`)
- `--reconcile-frequency` — how often to reconcile all tenants (default `10m`)

## Build

```bash
make build                       # Build Go binary
make podman-build                # Build container image
make podman-push                 # Push container image
```

## Helm Chart

```bash
make helm-deploy                 # helm upgrade --install maas-tokenizer ./chart
```

### Chart values (values.yaml)

```yaml
maasUrl: ""                      # Required — MaaS API base URL
maasToken: ""                    # Required — OpenShift SA bearer token
tokenExpiry: "24h"               # MaaS token expiration
reconcileFrequency: "10m"        # Reconciliation interval
```

### RBAC

The chart creates a ClusterRole with:

- `namespaces` — get, list, watch, update, patch
- `secrets` — get, list, create, update, patch
- `deployments` (apps) — get, list, patch
