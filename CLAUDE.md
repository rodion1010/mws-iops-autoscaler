# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# MWS Cloud Platform — reference for the IOPS autoscaler

Facts only. `[V]` = verified against a primary source, `[?]` = unknown.
Scope: MWS Cloud Platform (`mws.ru/docs/cloud-platform/**`).

---

## Disk NBS-PL2

`https://mws.ru/docs/cloud-platform/compute/general/disks-overview.html`

| | |
|---|---|
| Type | `NBS-PL2` (only network disk type) |
| IOPS range | 1000–10000 per disk |
| Bandwidth | `MB/s = IOPS × 32 / 1024` → 31 MB/s @1000, 310 MB/s @10000 |
| Size | 1 GB – 16 TB, 1 GB step; grow only |
| Perf change | live; no detach, no VM stop |
| Block transport | virtio-scsi |

## IOPS limits [V]

`https://mws.ru/docs/cloud-platform/compute/general/disks-iops-limits.html`

- VM ceiling: **3000 IOPS per vCPU**. 4 vCPU → 12000.
  The spec defines `GET /compute/v1/vmTypes/{vmType}` → `spec.disks.totalIops`, but **the API returns
  `spec.disks: null` on every VM type** (verified on `base-2-8`, `base-2-4`, `base-4-16`, `gen-4-16`,
  and all 40 in the list response). Only `spec.cpu.vcpuCount` is populated, so the ceiling must be
  computed as `3000 × vcpuCount` until the field is served.
- Effective disk performance = whichever limit binds first («ограничение, которое достигается раньше»).
  Docs example: 2 vCPU (6000 pool), two 6000-IOPS disks → one active disk gets 6000, both active get
  3000 each. A disk limit above its fair share is useful (borrows the idle pool); above the whole
  pool it is pure waste.
- Disk limit = base IOPS (in price) + additional (billed).
- VM pool split evenly across disks; unused remainder redistributed evenly among the rest.
- Disk IOPS limit is a maximum, not a guarantee.
- Local disks: `50000 × namespace`, namespace = 248 GB. Not NBS-PL2.
- Docs rule: disk limit unsaturated + stalled perf → add vCPU. VM limit unsaturated + disk maxed →
  raise disk IOPS.

---

## API

- Base endpoint: `https://api.mwsapis.ru` (Go SDK default)
- **Compute host: `compute.mwsapis.ru`**
- **Monitoring host: `monitoring.mwsapis.ru`**
- Auth: `Authorization: Bearer <IAM-token>` → 401 on failure
- OpenAPI: `github.com/mws-cloud-platform/api` → `openapi/<service>/openapi.gen.yaml`
  (mirror `gitverse.ru/mws-cloud-platform/api`)
  Services: `cdn certmanager compute gpt iam kms mk8s mkafka mpostgres registry rm secretmanager vi vpc`

### Disk operations [V]

| Operation | Method | Path |
|---|---|---|
| ListDisks | GET | `/compute/v1/projects/{project}/disks` |
| GetDisk | GET | `/compute/v1/projects/{project}/disks/{disk}` |
| UpsertDisk | POST | `/compute/v1/projects/{project}/disks/{disk}` |
| DeleteDisk | DELETE | `/compute/v1/projects/{project}/disks/{disk}` |

- `UpsertDisk` returns `DiskResponse` 200 (synchronous, not a long-running Operation).
- ListDisks filter/sort fields: `spec.zone`, `spec.size`, `status.ready.state`, `status.storageSize`,
  `status.diskType`, `status.blockSize`, `status.iops.total`.

### `DiskSpecRequest` mutability [V]

From `openapi/compute/openapi.gen.yaml` (clone the repo — the web view truncates at ~1000 lines).
Immutable fields carry the note "Неизменяемое поле. Задается только при создании ресурса.
При обновлении ресурса можно передать только текущее значение".

| Field | Mutable on update |
|---|---|
| `iops` | **yes** — no immutability note |
| `size` | **yes** (grow only) |
| `zone`, `source`, `diskType`, `blockSize`, `encryption` | no — may only resend the current value |
| `osType` | no note |

`Iops` is `type: integer, format: int64` with **no min/max in the schema** — the 1000–10000 bound is
not enforced by the spec.

### Guest device mapping [V]

`https://mws.ru/docs/cloud-platform/compute/general/disks-attach-detach-change-spec-to-ref.html`

Deterministic, no serial-number probing needed:

```
/dev/disk/by-id/mws-{deviceName}   # when deviceName is set
/dev/disk/by-id/mws-{name}         # when it is not
```

Spec text: "Уникальное имя устройства, которое отображается в дереве /dev/disk/by-id/mws-* Linux.
Если не указано - «mws-{name}», если указано - «mws-{deviceName}»".

`StorageDiskStatusResponse.deviceName` is a **required** response field, so the value is read from the
API rather than reconstructed. That schema also carries `name` (alias unique within the VM), `boot`,
`ref`, `ready`, `iops`, `size`.

### Change IOPS [V]

`https://mws.ru/docs/cloud-platform/compute/general/disks-performance-edit.html`

```bash
curl -X POST 'https://compute.mwsapis.ru/compute/v1/projects/<project>/disks/<disk>?updateOnly=true' \
  -H "Authorization: Bearer <IAM-token>" \
  -H "Content-Type: application/json" \
  -d '{"spec": {"iops": <N>}}'
```

- Partial update: body carries `spec.iops` only.
- `?updateOnly=true` prevents create-on-miss.
- CLI, independent disk: `mws compute disk update <disk> --iops <N>`
- CLI, dependent disk (full spec required):

```bash
mws compute vm update <vm> \
  --storage-disks 'name: <disk>
    managed: true
    disk:
      spec:
        diskType: <current>
        iops: <new>
        size: <current>'
```

Reference pages: `https://mws.ru/docs/cloud-platform/api/compute/operations/UpsertDisk.html`
(also `ListDisks.html`, `GetDisk.html`, `DeleteDisk.html`)

---

## VM service-account token [V]

`https://mws.ru/docs/cloud-platform/compute/general/vm-link-sa.html`

```bash
curl -s -H "Metadata-Flavor: Google" \
  http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token
```

```json
{ "access_token": "ey...Dw", "expires_in": 43144, "token_type": "Bearer" }
```

- Header `Metadata-Flavor: Google` mandatory.
- `expires_in` ≈ 43144 s (~12 h) in the docs example.
- Attach SA: `mws compute vm update <vm> --service-account <sa>`

---

## Monitoring [V]

`https://mws.ru/docs/cloud-platform/monitoring/general/obtain-metrics.html`
`https://mws.ru/docs/cloud-platform/monitoring/general/whatis-monitoring.html`

```
GET https://monitoring.mwsapis.ru/metrics/v1/projects/<project>/serviceMetrics/<service>:exportPrometheusMetrics
```

- Auth: `Authorization: Bearer <API-key>`; SA role **`monitoring.metricExporter`**.
  API key, not the metadata IAM token — different credential type from the disk API.
- Response: Prometheus exposition format.
- Covered services: Object Storage, Compute, CDN, Artifact Registry, Managed Kubernetes,
  Managed PostgreSQL, Managed ClickHouse, Managed Kafka.
- Metric naming pattern: `storage_bucket_object_count`, labels `bucket_name`, `object_type`,
  `project_name`, `service`.
- Exportable metric set is fixed and cannot be extended.

```yaml
scrape_configs:
  - job_name: 'mws-metrics'
    metrics_path: '/metrics/v1/projects/<project_name>/serviceMetrics/<service_name>:exportPrometheusMetrics'
    scheme: 'https'
    bearer_token_file: '<file with API key>'
    static_configs:
    - targets: ['monitoring.mwsapis.ru']
```

### Prometheus export — measured against a live project [V]

Verified 2026-08-28 on `example-project` with a `base-2-8` VM, two NBS-PL2 disks at
1000 IOPS, under `fio randread 4k iodepth=64 numjobs=4`.

- `<service>` = **`compute`**. Invalid names return `400 validating 'service': invalid service name`.
- Auth: **API key**, not the IAM token. An IAM token returns a misleading
  `503 "export service: internal error / unexpected status code: 404"` that hides the real cause;
  the API key returns a clean `403 ACCESS_DENIED permission='monitoring.metric.export'`.
  Role: `monitoring.metricExporter`. It is **not** grantable from the CLI (`iam role` has only
  `get`/`list`, and `roleBindings`/`accessBindings`/`getIamPolicy` all 404) — console only.
- **No `# HELP` / `# TYPE` lines** in the output. Metric semantics must be inferred.

Labels on every disk metric: `device_name`, `disk_name`, `host_name`, `project_name`, `service`,
`vm_name`.
`device_name` carries the **`mws-` prefix** — `device_name="mws-datadisk"` for `deviceName: datadisk`,
so it joins directly to `/dev/disk/by-id/mws-datadisk`. `disk_name` is the generated cloud resource
name (e.g. `iops-test-vm-mza3njax`), not the deviceName.

| Metric | Notes |
|---|---|
| `compute_vm_disk_read_ops` / `_write_ops` | **average** over the scrape window, fractional |
| `compute_vm_disk_read_ops_peak` / `_write_ops_peak` | **peak** over the window |
| `compute_vm_disk_read_bytes` / `_write_bytes` (+`_peak`) | throughput, same avg/peak split |
| `compute_vm_disk_read_latency_hist_bucket` / `_count` / `_sum` | **histogram**, not quantiles; `le` in **nanoseconds** |
| `compute_vm_disk_write_latency_hist_*` | same |
| `compute_vm_disk_write_queue_length` / `_max` | write queue only — **no read queue metric exists** |
| `compute_vm_disk_ops_peak_quota_utilization` | **read 0 even at full saturation — do not use** |
| `compute_vm_disk_bytes_peak_quota_utilization` | same, reads 0 |

Non-disk VM metrics also exported: `compute_vm_network_{received,sent}_{bytes,packets}`,
`compute_vm_open_sessions`, `compute_vm_session_limit`, `compute_vm_session_dropped_packets`,
`compute_vm_firewall_dropped_packets`, `compute_vm_egress_nat_dropped_packets`.

### Timing, measured [V]

Two controlled runs (constant 500 IOPS via `fio --rate_iops=500`, and full saturation), sampled
every 10 s for ~5 min each, cross-checked against `read_latency_hist_count` deltas:

| Property | Value |
|---|---|
| **Publication interval** | **exactly 30 s** (all `metric_ts` deltas are multiples of 30000 ms) |
| **Averaging window** | **trailing 60 s** — windows overlap 50% |
| **Publication lag** | **62–90 s**, mean 75 s (wall clock minus `metric_ts`) |
| `_peak` | max within the same 60 s window |

The 60 s window is exact, not approximate: every steady-state sample matched
`(hist_count[t] − hist_count[t−60s]) / 60` to within 0.5 ops/s, while the 30 s calculation did not.

**At steady state the average and the peak agree.** Saturated at the 1000 IOPS limit:
`read_ops = 999.9–1000.3`, `read_ops_peak = 1001–1002`. At a constant 500: `read_ops = 499.8–500.1`,
`read_ops_peak = 500–519`. Ratio peak/avg ≈ 1.01.

**They diverge only during load transitions**, because the 60 s trailing window still contains the
previous regime. Measured on the ramp into saturation: `read_ops` went 636 → 1136 → 999.9 across
three publications while `read_ops_peak` sat at 1001 from the first one. The 1136 overshoot is the
window straddling idle and full load.

→ **`_peak` reacts within one publication; the average takes up to 60 s longer.** Prefer `_peak` for
scale-up decisions. The average is not wrong, just slower.

→ **Worst-case detection latency ≈ 75 s lag + 60 s window ≈ 135 s.** This is the hard floor on dwell
time: no decision can be based on data younger than this, and any dwell shorter than ~3 min is
measuring noise.

**The quota-utilization metrics look purpose-built for this and are broken.** Both read `0` on a disk
whose peak equals its limit. Utilization must be computed as `read_ops_peak / spec.iops` instead.

**Counter cross-check:** guest `/proc/diskstats` showed 960744 reads on `sdb`;
`compute_vm_disk_read_latency_hist_count` showed 960770. The platform counts the same operations the
guest does, so guest-side and platform-side collection are interchangeable for op counts.

### VM metrics [V]

`https://mws.ru/docs/cloud-platform/compute/general/vm-monitoring.html`

CPU utilization %; read/write throughput; read/write op counts — per disk and aggregated.
Exact values retained 2 days, then aggregated. Console refresh 15 s.

### Disk metrics [V]

`https://mws.ru/docs/cloud-platform/compute/general/disks-monitoring.html`

| Metric | Detail |
|---|---|
| Read throughput | avg + max over period |
| Write throughput | avg + max over period |
| Read op count | avg + max over period |
| Write op count | avg + max over period |
| Read latency | quantiles 50/75/90/99, ms |
| Write latency | quantiles 50/75/90/99, ms |
| Write queue depth | outstanding I/O requests awaiting service |

No `%util`, no throttling counter. Read queue not listed.

---

## Billing [V]

`https://mws.ru/docs/cloud-platform/compute/general/pricing.html`

- Disk price = type + size (GB) + selected IOPS. **Provisioned, not consumed.**
- NBS-PL2: 1000 IOPS included in base price; pay only above that.
- IOPS sold in packages of **100**.
- **Metering is per-minute; rates are quoted per hour** (unit `шт.×час`). Confirmed by the product
  team, not stated in the docs — the pricing page only shows a whole-hour worked example
  (10 h @ 1000 IOPS, then 20 h @ 5000 → `(5000−1000)/100 × 20 h = 800 шт.×час`) and never addresses
  a mid-hour change.
- VMs billed per minute. Stopped VM not billed; its disks and added IOPS still are.

---

## Go SDK

`github.com/mws-cloud-platform/go-sdk` · `go.mws.cloud/go-sdk` · `pkg.go.dev/go.mws.cloud/go-sdk`

`go get go.mws.cloud/go-sdk` — under active development, breaking changes possible.
Dirs: `examples/ internal/ mws/ pkg/ service/`. `service/compute/` → `client/` (+`http/`, `mocks/`),
`model/`, `sdk/`.

| Env var | Default |
|---|---|
| `MWS_BASE_ENDPOINT` | `https://api.mwsapis.ru` |
| `MWS_PROJECT` | — |
| `MWS_ZONE` | `ru-central1-a` |
| `MWS_TOKEN` | — |
| `MWS_SERVICE_ACCOUNT_AUTHORIZED_KEY_PATH` | — |
| `MWS_TIMEOUT` | `5s` |
| `MWS_LOG_LEVEL` | `debug`/`info`/`warn`/`error`/`fatal` |
| `MWS_TRACE_ENABLED` | — |

```go
sdk, err := mws.Load(ctx,
    mws.WithDefaultProject("my-project"),
    mws.WithDefaultZone("ru-central1-a"),
    mws.WithTimeout(5*time.Second),
)

sdk, err := mws.Load(ctx, mws.WithCredentials(
    credentials.StaticProvider(credentials.Credentials{AccessToken: "mws-iam-token"}),
))

key, err := iam.ServiceAccountAuthorizedKeyFromFile("/path/to/key.json")
sdk, err := mws.Load(ctx, mws.WithServiceAccountAuthorizedKey(key))

client, err := computesdk.NewVirtualMachine(ctx, sdk)
vms, err := client.ListVirtualMachines(ctx, computeclient.ListVirtualMachinesRequest{PageSize: new(10)})
```

Credential sources: static token, SA authorized key, custom provider, Instance Metadata Service.

---

## Terraform provider

`github.com/mws-cloud-platform/terraform-provider-mws` · `docs/resources/compute_disk.md`

```hcl
resource "mws_compute_disk" "disk" {
  disk      = var.disk_name
  disk_type = "diskTypes/nbs-pl2"
  iops      = 1000
  size      = "10GB"
}
```

- Required: `disk`.
- Optional: `iops`, `size`, `block_size` (units `B KB MB GB TB PB EB ZB YB RB QB`), `disk_type`,
  `encryption.crypto_key_id`, `kind`, `metadata`, `os_type`, `project`,
  `source` (`disk_backup`/`image`/`snapshot`), `timeouts`, `zone`.
- Read-only: `id`, `status` (→ `linked_vms`, `ready`, IOPS and throughput breakdown).
- `[?]` ForceNew/RequiresReplace behaviour undocumented.

---

## Org repositories

`github.com/mws-cloud-platform`

| Repo | Description |
|---|---|
| `api` | MWS Cloud OpenAPI |
| `go-sdk` | Official Go SDK |
| `terraform-provider-mws` | Terraform provider |
| `packer-plugin-mws` | Packer plugin, Compute Builder |
| `gpu-metrics-exporter` | VM GPU metrics exporter (Go, systemd, vsock guest→host, deb) |
| `util-toolset` | internal tooling |
| `skills` | AI skills library (`mws-cli`) |

---

## Test environment [V]

Project `example-project`, CLI profile `iops-autoscaler` (`client_timeout: 60s`).

| Resource | Detail |
|---|---|
| `iops-test-net` / `iops-test-subnet` | `10.10.0.0/24`, internet access on |
| `allow-ssh` firewall rule | INGRESS ALLOW TCP:22, priority 1000 |
| `iops-test-vm` | `base-2-8` (2 vCPU → 6000 IOPS pool), Ubuntu 24.04, SA `claude` attached |
| disks | `bootdisk` 20 GB, `datadisk` 10 GB, both 1000 IOPS |
| access | `ssh -i secrets/id_ed25519 claude@203.0.113.10` |

**A new network denies all inbound by default** — `fr-wuqpuv` INGRESS DENY `0.0.0.0/0` at priority
65535, with only RFC1918 allowed. TCP connects (the NAT answers) but the SSH banner never arrives,
which reads like a network fault rather than a firewall. An explicit ingress rule is required.

`admin` and `root` are rejected as cloud-init usernames.

Guest device mapping confirmed live: `/dev/disk/by-id/mws-datadisk → sdb`. A second undocumented
alias also exists: `/dev/disk/by-id/scsi-0MWS_PersistentDisk_mws-<deviceName>`.

Metadata token confirmed working from inside the guest.

## Open questions

1. Metric granularity, publication lag, and retention in the API (15 s is console-only). The scrape
   window length matters — it sets how far `_peak` and the average diverge.
2. Rate limit / cooldown on disk modification.
3. Behaviour when `iops` exceeds the VM ceiling — reject, clamp, or accept and bill. The schema
   imposes no bound, so this is decided server-side.
4. Disk client in the Go SDK (`service/compute/sdk`, `service/compute/client`).
5. Whether `*_quota_utilization` is expected to work — it reads 0 at full saturation.

Closed: `DiskRequest`/`DiskSpecRequest` schema and mutability; guest device mapping; Prometheus
metric names, labels and semantics; `<service>` value; read-queue question (none exists);
monitoring auth model.

---

## Algorithm references

- AWS RDS storage autoscaling —
  `docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIOPS.Autoscaling.html`
  Dwell time ≥ 5 min; ≤ 4 modifications per 24 h; hard ceiling `MaxAllocatedStorage`;
  step = `max(10 GiB, 10% of current, predicted 7 h growth)`; never scales down.
- Azure MySQL Flexible Server autoscale IOPS —
  `learn.microsoft.com/en-us/azure/mysql/flexible-server/concepts-storage-iops`
  Signal: Storage IO Percent. Billing per consumed operation (inverse of MWS).
