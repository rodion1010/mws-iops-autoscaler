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
  **Do not hardcode this — the API serves it.** `GET /compute/v1/vmTypes/{vmType}` →
  `spec.disks.totalIops` ("Максимальное количество операций ввода-вывода в секунду (IOPS) на ВМ.
  Зависит от количества виртуальных процессорных ядер (vCPU) у ВМ"). vCPU count itself is
  `spec.cpu.vcpuCount`.
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

## Open questions

1. Metric names and label sets in the Prometheus export; `<service>` value for Compute; whether a
   read-queue metric exists. → one request to `monitoring.mwsapis.ru`.
2. Metric granularity, publication lag, and retention in the API (15 s is console-only).
3. Rate limit / cooldown on disk modification.
4. Behaviour when `iops` exceeds the VM ceiling — reject, clamp, or accept and bill. The schema
   imposes no bound, so this is decided server-side.
5. Disk client in the Go SDK (`service/compute/sdk`, `service/compute/client`).

Closed: full `DiskRequest`/`DiskSpecRequest` schema and field mutability; guest device mapping.

---

## Algorithm references

- AWS RDS storage autoscaling —
  `docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIOPS.Autoscaling.html`
  Dwell time ≥ 5 min; ≤ 4 modifications per 24 h; hard ceiling `MaxAllocatedStorage`;
  step = `max(10 GiB, 10% of current, predicted 7 h growth)`; never scales down.
- Azure MySQL Flexible Server autoscale IOPS —
  `learn.microsoft.com/en-us/azure/mysql/flexible-server/concepts-storage-iops`
  Signal: Storage IO Percent. Billing per consumed operation (inverse of MWS).
