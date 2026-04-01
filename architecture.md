# Architecture

## Overview

The vertical disk autoscaler is a Kubernetes controller built with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime). It combines an event-driven controller for configuration management with a periodic loop for metrics-based scaling decisions.

```
┌─────────────────────────────────────────────────────┐
│                  Kubernetes API                      │
│  DiskAutoscalePolicy    PVC    PV    StorageClass    │
└──────────┬──────────────┬───────┬───────────────────┘
           │              │       │
     ┌─────▼──────────────▼───────▼──────┐
     │       Policy Controller           │
     │       (event-driven)              │
     │                                   │
     │  - Watches DiskAutoscalePolicy    │
     │  - Watches PVCs                   │
     │  - Resolves PVC → PV → Disk ID   │
     │  - Maintains disk registry        │
     └──────────────┬────────────────────┘
                    │
              ┌─────▼─────┐
              │  Registry  │
              │ (in-memory)│
              └─────┬──────┘
                    │
     ┌──────────────▼───────────────────┐
     │        Scaling Loop              │
     │        (periodic, every 30s)     │
     │                                  │
     │  For each managed disk:          │
     │  1. Fetch Azure Monitor metrics  │
     │  2. Evaluate scaling decision    │
     │  3. Apply via Azure Compute API  │
     │  4. Update CRD status            │
     └──────────┬───────────┬───────────┘
                │           │
        ┌───────▼───┐  ┌───▼────────────┐
        │  Azure    │  │  Azure         │
        │  Monitor  │  │  Compute       │
        │  (read)   │  │  (write)       │
        └───────────┘  └────────────────┘
```

## Components

### CRD: DiskAutoscalePolicy

The custom resource defines scaling behavior. It is cloud-agnostic -- the controller infers the cloud provider from the PV's CSI driver field.

Key sections:
- **Target selection**: `pvcSelector` (label-based) and/or `storageClassName`
- **Scale up/down**: Independent configuration per direction with separate targets, steps, and cooldowns
- **Constraints**: Min/max bounds for IOPS and throughput
- **Behavior**: Initialization period (no scaling after first seeing a disk) and metrics averaging window
- **Rate limit**: Per-disk cap on scaling actions per hour
- **Status**: Per-disk state including current performance, utilization, phase, and timestamps

### Policy Controller

Standard controller-runtime reconciler. Watches:
- `DiskAutoscalePolicy` (primary)
- `PersistentVolumeClaim` (secondary, mapped back to policies)

On each reconcile:
1. Lists PVCs matching the policy's selector/StorageClass
2. For each bound PVC, resolves the PV to get the Azure disk resource ID from `spec.csi.volumeHandle`
3. Registers/unregisters disks in the in-memory registry
4. Cleans up disks that no longer match

### Registry

Thread-safe in-memory store (`sync.RWMutex` + map) of managed disks. Tracks:
- PVC/PV/disk ID mapping
- Policy reference
- Current provisioned IOPS/throughput
- Scaling history (for rate limiting)
- Last scale timestamps (for cooldown)
- Phase (Initializing, Idle, ScalingUp, ScalingDown, CooldownUp, CooldownDown, Error)

The registry preserves scaling state across policy re-reconciliations. If a PVC is re-registered (e.g., policy labels change), accumulated state like `FirstSeen` and `ScaleHistory` is preserved.

### Scaling Loop

Implements `manager.Runnable`. Runs a tick-based loop at a configurable interval (default 30s).

Each tick, for every managed disk:

1. **Fetch performance** -- if not yet known, reads current IOPS/throughput from Azure Compute
2. **Fetch metrics** -- queries Azure Monitor for IOPS and throughput utilization over the configured window
3. **Evaluate** -- calls the pure decision function with current state and policy spec
4. **Apply** -- if a scale action is returned, updates the disk via Azure Compute API
5. **Update status** -- writes metrics and phase back to the CRD status subresource

### Scaling Decision Logic

Pure function in `internal/scaler/decision.go` with no I/O dependencies:

```go
func Evaluate(state DiskState, spec DiskAutoscalePolicySpec, now time.Time) *ScaleAction
```

Decision rules:
- **Initialization period**: No scaling within `initializationPeriod` of first seeing a disk
- **Rate limit**: No scaling if `maxScalesPerHour` is exceeded
- **Scale up**: Triggers if IOPS *or* throughput utilization exceeds the upscale target. Steps are applied independently per dimension.
- **Scale down**: Triggers only if *both* IOPS and throughput are below the downscale target (conservative). Both dimensions are stepped down together.
- **Clamping**: Results are clamped to the configured min/max bounds
- **IOPS-throughput coupling**: Azure requires `throughput <= IOPS * 0.25`. If throughput needs more IOPS, they're bumped. If IOPS can't cover the throughput (would exceed max), throughput is capped instead.
- **Cooldown**: Separate cooldowns for scale-up and scale-down, checked against last action timestamps
- **No-op detection**: If the computed new values equal current values (already at min/max), no action is taken

### Azure Provider

Wraps two Azure SDK clients:

**Monitor client** (`armmonitor.MetricsClient`):
- Queries `Composite Disk Read/Write Operations/sec` and `Composite Disk Read/Write Bytes/sec`
- Averages over the configured metrics window
- Computes utilization as percentage of provisioned capacity

**Compute client** (`armcompute.DisksClient`):
- Reads current disk properties (IOPS, throughput)
- Updates IOPS and throughput in a single PATCH call
- Uses `BeginUpdate` + `PollUntilDone` for the async operation

**Rate limiting**:
- Global `rate.Limiter` (from `golang.org/x/time/rate`) wraps all write operations
- Configured via `--max-azure-writes-per-hour` flag (default 600, well below Azure's 1200/hr limit)
- Burst of 5 to handle concurrent scaling of multiple disks

### Disk ID Resolution

The path from PVC to Azure disk:

```
PVC.spec.volumeName → PV name
PV.spec.csi.driver  → "disk.csi.azure.com" (confirms Azure)
PV.spec.csi.volumeHandle → /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Compute/disks/{name}
```

The resource ID is parsed to extract subscription ID, resource group, and disk name for API calls. The subscription ID is embedded in the resource ID, so no separate configuration is needed.

## Metrics source

The controller uses Azure Monitor as its metrics source. This provides per-disk utilization metrics with ~1-2 minute lag, which is acceptable for the autoscaler's use case (multi-hour database restores, not sub-second spikes).

**Future: Prometheus support**. For faster reaction times, a Prometheus metrics source could be added using node-exporter disk I/O metrics. This requires mapping PVC → PV → Azure Disk → LUN ID → block device on node → node-exporter metrics, which adds significant complexity. The `DiskProvider` interface could be extended with a pluggable metrics source.

## Authentication

The controller uses `azidentity.NewDefaultAzureCredential()`, which tries multiple authentication methods in order. The recommended setup for AKS is Azure Workload Identity, which federates a Kubernetes ServiceAccount with an Azure Managed Identity.

Required Azure RBAC roles:
- `Disk Pool Operator` (or custom: `Microsoft.Compute/disks/read` + `Microsoft.Compute/disks/write`)
- `Monitoring Reader` (for Azure Monitor metric queries)

## Cloud provider abstraction

The CRD and scaling logic are cloud-agnostic. The Azure-specific code is isolated in `internal/azure/`. Adding GCP Hyperdisk support requires:

1. A new `internal/gcp/` package implementing the same operations (get performance, set performance, get metrics, resolve disk ID)
2. Detection in the policy controller: check `pv.Spec.CSI.Driver == "pd.csi.storage.gke.io"` and route to the GCP provider
3. GCP-specific constraint validation (different IOPS/throughput limits and coupling rules)

No changes to the CRD, scaling logic, registry, or controller structure are needed.
