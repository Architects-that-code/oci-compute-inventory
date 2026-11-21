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
make image IMAGE=iad.ocir.io/NAMESPACE/span-the-globe/oci-compute-inventory:v0.1.0
```

`make image` builds Linux AMD64 and ARM64 into `dist/collector.oci.tar`, without
publishing. The pinned Docker build stage compiles a static Go executable; the
scratch runtime contains only `/collector` and CA certificates, running as UID
65532. Kubernetes mounts the service-account token and writable `/tmp`.

## Publish a release

Configure the OCI CLI with your normal API-key profile or OCI_CLI_* environment
variables. Set `OCI_CLI_REGION=us-ashburn-1` for IAD. The operator needs repository
push permission. Create a private OCIR repository before publishing, either through
your infrastructure project or the OCI CLI:

```bash
oci artifacts container repository create --compartment-id COMPARTMENT_OCID \
  --display-name span-the-globe/oci-compute-inventory --is-public false
make publish IMAGE=iad.ocir.io/NAMESPACE/span-the-globe/oci-compute-inventory:v0.1.0
```

Publishing runs all tests first, builds both architectures, and pushes the image.
A temporary Docker configuration holds a short-lived OCIR bearer token and is
removed on exit. Existing release tags are reused: always choose a new tag for
changed source. Keep tags immutable, or deploy the published manifest digest.
Nothing builds or publishes during a Terraform apply.

## Runtime and authentication

The default authentication is OKE workload identity, using a shared OCI Go SDK
provider. Deploy as service account `monitoring/oci-compute-inventory` with IAM
scoped to that account, namespace, and cluster: `read instances`, `inspect
compartments`, and `inspect tenancies`. No operator credentials belong in the pod.
Private image pulls need the node's OCIR credential provider and repository read
permission. See [OKE workload identity](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contenggrantingworkloadaccesstoresources.htm).

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

Set `compute_inventory_image` in 003 to the published image tag or digest, then
apply 003. Its Helm chart starts `/collector` directly. The infrastructure project
continues to manage IAM, OCIR infrastructure, scraping, and Grafana provisioning.
