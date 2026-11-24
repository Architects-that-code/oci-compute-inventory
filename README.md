# OCI Compute inventory

Standalone Go collector for running Compute instance counts across every READY
subscribed OCI region and ACTIVE compartment, including the tenancy root. It can
also collect instance health, Compute Agent coverage, and active maintenance.

## Grafana dashboards

Import the desired JSON files from `dashboards/` into Grafana with a Prometheus
datasource UID of `prometheus`. The dashboards share region and compartment
filters and link to one another.

- `oci-global-compute-inventory.json` shows counts and scan status.
- `oci-global-compute-inventory-map.json` adds a regional map. Its count queries
  expect the scrape job `oci-compute-inventory`. For map markers, Prometheus also
  needs one `oci_region_info` series of value 1 per region with `region`,
  `latitude`, and `longitude` labels. The dashboard can also show `city`,
  `region_key`, `display_name`, and `country_code`. Supply these series through
  the Prometheus setup that provides your region metadata.
- `oci-compute-instance-operations.json` shows health issues, active maintenance,
  missing Compute Agent telemetry, and failed OCI source queries.

## Build and test

Prerequisites: Go 1.26 or newer (race tests require a working C compiler), Python 3
for dashboard validation, and Docker with Buildx for image builds. No cloud credentials are needed for tests or builds.

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

Pushes to `main` automatically run `make build` (including race tests, vet, and
dashboard validation), then publish a new image tagged `sha-<full-commit-SHA>`.
Pull requests targeting `main` run the same build and tests. The publish job
depends on successful build and test completion; failed checks prevent publishing.
Images include both Linux AMD64 and ARM64. Re-running the same commit reuses its
existing image tag. After successful publishing on `main`, the workflow also
updates `ghcr.io/architects-that-code/oci-compute-inventory:latest` to the same
multi-architecture image. Manual releases do not move `latest`.

For a versioned release, run **Actions → Publish container → Run workflow** and
enter a new release version. The workflow tests and publishes both architectures with its
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
an alternative repository under ghcr.io. OCI CLI credentials are not needed to publish.

New GHCR packages are private by default. Before deploying, set the package's visibility to **public** in GitHub package settings so
cluster nodes can pull anonymously. OCI node dynamic groups do not grant access
to private GHCR images. See [GitHub Container Registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

## Runtime and authentication

The default authentication is OKE workload identity, using a shared OCI Go SDK
provider. Deploy as service account `monitoring/oci-compute-inventory` with IAM
scoped to that account, namespace, and cluster: `read instances`, `inspect
compartments`, and `inspect tenancies`. No operator credentials belong in the pod.
The default deployment pulls the public GHCR package anonymously. See [OKE workload identity](https://docs.oracle.com/en-us/iaas/Content/ContEng/Tasks/contenggrantingworkloadaccesstoresources.htm).

Required environment: `OCI_TENANCY_OCID` and, in workload mode,
`OCI_RESOURCE_PRINCIPAL_VERSION=2.2` (required by the OCI Go SDK). Optional settings:
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

For local validation only, supply an explicit OCI properties file outside this
checkout using the standard OCI SDK configuration format:

```ini
[DEFAULT]
tenancy=ocid1.tenancy.oc1..REPLACE
user=ocid1.user.oc1..REPLACE
fingerprint=REPLACE
key_file=/absolute/path/outside/repository/api-key.pem
region=us-ashburn-1
```

```bash
dist/collector --once --auth config --config-file /secure/path/oci.properties
# Add --profile NAME for a named profile.
```

The tenancy comes from that file in config mode. Config authentication is rejected
for the long-running service. There is no automatic credentials-file discovery or
Terraform-variable authentication. Keep the file and private key outside the
checkout, restrict their filesystem permissions, and never commit, bake into an
image, or deploy them. Git ignores `*.properties`, `*.pem`, `*.key`, `.oci/`, and
`oci-config*`; the Docker build context allows only Go source and module files.

## Container deployment

Run the published image with port 9108 exposed and `OCI_TENANCY_OCID` set.
Select identity explicitly; there is no fallback to local credentials:

| Runtime | Arguments | Identity setup |
| --- | --- | --- |
| OKE pod | `--auth workload` (default) | Workload identity and IAM policy scoped to the service account, namespace, and cluster |
| Container on OCI Compute | `--auth instance` | Host instance principal, dynamic group and IAM policies; container must reach instance metadata |
| OCI Container Instances | `--auth resource` | Enable resource principal access for the container; configure dynamic group and IAM policies |

A host outside OCI needs an OCI runtime identity to run the deployed service;
file credentials are reserved for local one-shot validation. Do not mount local
properties files or API keys into deployed containers. Grant the chosen principal
`read instances`, `inspect compartments`, and `inspect tenancies` for the intended
tenancy scope. Configure Prometheus to scrape `/metrics` on port 9108. Verify
`oci_compute_inventory_collection_success == 1` and snapshot freshness.

See [OCI SDK authentication](https://docs.oracle.com/en-us/iaas/Content/API/Concepts/sdk_authentication_methods.htm)
and [Container Instances resource principal access](https://docs.oracle.com/en-us/iaas/tools/go/latest/containerinstances/index.html).

## Instance operations

Add `--instance-monitoring` to the collector arguments to enable the additional
read-only queries. The flag also works with `--once`. In addition to the
inventory permissions, grant the existing runtime identity `read metrics` and
`inspect instance-maintenance-events` in the monitored scope. See
[Monitoring permissions](https://docs.oracle.com/en-us/iaas/Content/Identity/Reference/monitoringpolicyreference.htm)
and [Compute permissions](https://docs.oracle.com/en-us/iaas/Content/Identity/Reference/corepolicyreference.htm#permissions-required-for-each-api-operation).

The collector queries OCI Monitoring in each region and compartment containing
running instances, joins by instance OCID, and reads all maintenance event
pages. Operations collection has its own `COLLECTION_TIMEOUT_SECONDS` budget
after inventory collection and uses up to `COLLECTOR_WORKERS` workers. If a source
query fails or reaches the 2,000-stream OCI response limit, the collector reports
the affected source and location. Counts and successful operations results
remain available.

Instance rows carry instance name, shape, availability domain, and fault
domain. The `owner` and `environment` labels come from the corresponding
lowercase OCI free-form tags when present. Maintenance rows show the OCI action,
window start and hard deadline as returned, with times displayed in UTC.
The telemetry coverage check looks for `oci_computeagent.CpuUtilization` in the
last 15 minutes. Evaluate telemetry gaps for workloads where that agent is
expected to run.

### Alert expressions

Use these PromQL conditions with the same fresh inventory check as the operations
dashboard. Route by workload ownership and choose a pending period appropriate
for the polling interval and workload.

| Condition | Expression |
| --- | --- |
| Latest observed health issue | `oci_compute_inventory_instance_health > 0` |
| Missing Compute Agent CPU sample | `oci_compute_inventory_instance_agent_cpu_samples_present == 0` |
| Scheduled disruptive maintenance due within 24 hours or overdue | `(oci_compute_inventory_instance_maintenance_deadline_timestamp_seconds{maintenance_state="SCHEDULED",maintenance_action=~"REBOOT_MIGRATION\|STOP\|TERMINATE"} - time()) < 86400` |
| Failed or uncompleted source query | `oci_compute_inventory_instance_signal_query_success == 0` |

The health series uses the latest returned OCI health point within the
15-minute query window. The coverage series is emitted only when the CPU query
succeeds. The maintenance deadline series is emitted when OCI supplies a hard
due date.

### Fleet operations dashboard and CPU values

The operations dashboard lists running instances with health, telemetry and
maintenance status, including healthy instances and instances with unknown
signals. The independent `oci_compute_inventory_instance_info` gauge preserves
all running inventory rows when monitoring sources fail. Filters cover region,
compartment, owner, environment and instance OCID. Source diagnostics use region
and compartment scope because each source query is shared by multiple instances.

`oci_compute_inventory_instance_cpu_utilization_percent` exports the latest
observed one-minute maximum returned by OCI Compute Agent within the 15-minute
lookback. `oci_compute_inventory_instance_cpu_sample_timestamp_seconds` is its
OCI source timestamp. Missing or failed CPU queries never emit a utilization
value of zero; use the existing coverage and query-success gauges to distinguish
missing telemetry from a query failure. Values are sampled at the collector
refresh cadence, not continuously streamed one-minute data.

Publish these changes with a new immutable release tag (`v0.1.0.2` for this
revision). Dashboard examples cover health issues, telemetry gaps, high CPU,
maintenance deadlines, source failures and collector availability. They do not
create alert rules; operators must set routing, workload thresholds, pending
periods and No Data / Error behavior. No new OCI API calls or IAM permissions
are required by these additional exported gauges.
