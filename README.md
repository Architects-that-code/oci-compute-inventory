# OCI Compute inventory

Standalone Go collector for running Compute instance counts across every READY
subscribed OCI region and ACTIVE compartment, including the tenancy root.
`dashboards/oci-global-compute-inventory.json` is a portable copy of the Grafana
dashboard with region and compartment filters. Its UID and Prometheus metric
contract match the dashboard deployed by span_the_globe 003. When changing it,
copy the reviewed JSON into that deployment repository's managed dashboard path.

## Build and test

Prerequisites: Go 1.26 or newer (race tests require a working C compiler), Python 3
for dashboard validation, and Docker with Buildx for image builds. No Terraform
checkout or credentials are needed for tests or builds.

```bash
make test
make build                           # dist/collector, native static binary
make image VERSION=v0.1.0
```

`make image` builds Linux AMD64 and ARM64 into `dist/collector.oci.tar`, without
publishing. The pinned Docker build stage compiles a static Go executable; the
scratch runtime contains only `/collector` and CA certificates, running as UID
65532. Kubernetes mounts the service-account token and writable `/tmp`.

## Publish a release

The default image is
`ghcr.io/architects-that-code/oci-compute-inventory:v0.1.0`, associated with the
[GitHub repository](https://github.com/Architects-that-code/oci-compute-inventory)
through the image's `org.opencontainers.image.source` label.

Authenticate with GitHub CLI (`gh auth login`) using a token that has
`write:packages`, or set `GHCR_TOKEN` (a classic PAT with `write:packages`) and
`GHCR_USERNAME` to the token owner's login. GitHub Actions can instead supply
`GITHUB_TOKEN` with `packages: write` and `GITHUB_ACTOR`. Do not put tokens in
Makefile arguments or source control.

Recommended: run **Actions → Publish container → Run workflow** and enter the
release version. The workflow tests and publishes both architectures with its
repository-scoped `GITHUB_TOKEN` and `packages: write`; no personal token is needed.
Repository visibility does not grant package push permission. For local publishing
with an existing GitHub CLI login, first run
`gh auth refresh -h github.com -s write:packages` to authorize that scope.

```bash
make publish VERSION=v0.1.0
```

Publishing runs all tests first, builds Linux AMD64 and ARM64, and pushes to GHCR.
A temporary Docker configuration holds authentication and is removed on exit.
Existing release tags are reused: always choose a new VERSION for changed source.
Keep tags immutable, or deploy the published manifest digest. Override IMAGE for
an alternative repository under ghcr.io. Nothing builds or publishes during a
Terraform apply, and OCI CLI credentials are not needed to publish.

New GHCR packages are private by default. Before deploying with the default 003
chart, set the package's visibility to **public** in GitHub package settings so
cluster nodes can pull anonymously. OCI node dynamic groups do not grant access
to private GHCR images. See [GitHub Container Registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

## Runtime and authentication

The default authentication is OKE workload identity, using a shared OCI Go SDK
provider. Deploy as service account `monitoring/oci-compute-inventory` with IAM
scoped to that account, namespace, and cluster: `read instances`, `inspect
compartments`, and `inspect tenancies`. No operator credentials belong in the pod.
The default deployment pulls the public GHCR package anonymously. See [OKE workload identity](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contenggrantingworkloadaccesstoresources.htm).

Required environment: `OCI_TENANCY_OCID`. Optional settings:
`OCI_RESOURCE_PRINCIPAL_REGION` (us-ashburn-1), `POLL_INTERVAL_SECONDS` (300),
`COLLECTOR_WORKERS` (4), and `COLLECTION_TIMEOUT_SECONDS` (600).
The HTTP listener exposes `/healthz` and `/metrics` on port 9108. Scans run at
startup and then pause for the configured poll interval after each completion.

Scans exhaust compartment and instance pagination, check RUNNING state,
deduplicate instance IDs, and publish counts atomically. Any discovery or scan
failure suppresses all count series and exposes collection failure diagnostics.
Counts include VM and bare-metal instances independently of OKE membership or
monitoring-agent state. OCI global inventory is observed over the scan duration,
not a transactional snapshot.

For read-only local operator validation, `dist/collector --once --auth environment`
accepts `TF_VAR_tenancy_ocid`, `TF_VAR_user_ocid`, `TF_VAR_api_fingerprint`, and
`TF_VAR_private_key_path`. This optional compatibility mode does not require a
Terraform project and does not validate the pod's workload IAM. After deployment,
verify `oci_compute_inventory_collection_success == 1` and snapshot freshness.

## Deploy with span_the_globe

003 defaults to the v0.1.0 GHCR image above. Set `compute_inventory_image` to a new
published image tag or digest when releasing an update, then
apply 003. Its Helm chart starts `/collector` directly. The infrastructure project
continues to manage IAM, Helm, scraping, and Grafana provisioning.
