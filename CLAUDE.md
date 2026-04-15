# rhai-maas-tokenizer

A Go service that watches for tenant namespaces on an OpenShift cluster and provisions MaaS (Model as a Service) credentials into each tenant namespace.

## How it works

1. **Discovers tenant namespaces** — finds all Namespaces with the `tenant` label (e.g. `user-rvzm9`).

2. **Obtains MaaS token** — calls `MAAS_URL/maas-api/v1/tokens` with the configured expiry, then `MAAS_URL/maas-api/v1/models` to discover available models.

3. **Creates secrets in per-app namespaces** — each tenant has app namespaces following the pattern `{tenant-namespace}-{app-prefix}`:
   - `{tenantNS}-demo`: Secret 'maas-secret'
   - `{tenantNS}-openclaw`: Secret 'maas-secret'

4. **Labels app namespaces** — after provisioning, labels each app namespace `rhai-tmm.dev/maas-auth: done` and annotates with `rhai-tmm.dev/maas-auth-until` (UTC RFC3339 expiry time computed from `--token-expiry`).

5. **Namespace watcher** — watches for namespaces with the `tenant` label and triggers immediate provisioning of their app namespaces.

6. **Periodic reconciliation** — re-runs every 10 minutes (configurable). Re-provisions any tenant whose app namespace tokens have expired (based on the `maas-auth-until` annotation) or are missing.

## Configuration

### Required environment variables

- `MAAS_URL` — MaaS API base URL (e.g. `https://maas.apps.example.com`)
- `MAAS_TOKEN` — MaaS user token for authentication
- `APP_NAMESPACES` — comma-separated list of app namespace suffixes (default `demo,openclaw`). Each tenant gets `{tenantNS}-{suffix}` namespaces provisioned.

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
maasToken: ""                    # Required — MaaS user token
tokenExpiry: "8h"                # MaaS token expiration
reconcileFrequency: "10m"        # Reconciliation interval
appNamespaces: "demo,openclaw"   # Comma-separated app namespace suffixes
```

### RBAC

The chart creates a ClusterRole with:

- `namespaces` — get, list, watch, update, patch
- `secrets` — get, list, create, update, patch
