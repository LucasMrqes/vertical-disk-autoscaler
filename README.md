# Vertical Disk Autoscaler

A Kubernetes controller that automatically adjusts IOPS and throughput on Azure Premium SSD v2 managed disks based on observed utilization. Scale up under load, scale down when idle -- pay only for what you use.

## How it works

1. You create a `DiskAutoscalePolicy` CR that selects PVCs by label or StorageClass
2. The controller watches matching PVCs, resolves them to Azure managed disks via the PV's CSI volume handle
3. A periodic loop fetches disk utilization from Azure Monitor, evaluates scaling decisions, and applies changes via the Azure Compute API
4. Disks scale up when utilization exceeds the target, and scale down when both IOPS and throughput are below the downscale target

## Use case

Restoring PostgreSQL databases from pgBackRest backups on AKS. Create the PVC with high IOPS/throughput for fast restore, then let the autoscaler downscale once the database is idle.

## Quick start

### Prerequisites

- AKS cluster with Azure Premium SSD v2 disks
- [Azure Workload Identity](https://learn.microsoft.com/en-us/azure/aks/workload-identity-overview) configured
- A Managed Identity with `Disk Pool Operator` + `Monitoring Reader` roles

### Install

```bash
helm install disk-autoscaler \
  oci://ghcr.io/lucasmrqes/vertical-disk-autoscaler/charts/vertical-disk-autoscaler \
  --version 0.1.0 \
  --namespace disk-autoscaler-system --create-namespace \
  --set serviceAccount.annotations."azure\.workload\.identity/client-id"="<your-client-id>"
```

### Create a policy

```yaml
apiVersion: disk.autoscaler.io/v1alpha1
kind: DiskAutoscalePolicy
metadata:
  name: default-policy
spec:
  storageClassName: managed-premium-v2

  scaleUp:
    enabled: true
    targetIOPSUtilizationPercent: 80
    targetThroughputUtilizationPercent: 80
    stepIOPS: 5000
    stepThroughputMBps: 100
    cooldown: 2m

  scaleDown:
    enabled: true
    targetIOPSUtilizationPercent: 30
    targetThroughputUtilizationPercent: 30
    stepIOPS: 2000
    stepThroughputMBps: 50
    cooldown: 10m

  constraints:
    minIOPS: 3000
    maxIOPS: 40000
    minThroughputMBps: 125
    maxThroughputMBps: 600

  behavior:
    initializationPeriod: 5m
    metricsWindow: 5m

  rateLimit:
    maxScalesPerHour: 6
```

## Configuration

### Controller flags

| Flag | Default | Description |
|------|---------|-------------|
| `--scaling-interval` | `30s` | How often the scaling loop runs |
| `--max-azure-writes-per-hour` | `600` | Global Azure API write rate limit |
| `--debug` | `false` | Enable debug logging |
| `--health-probe-bind-address` | `:8081` | Health/readiness probe address |
| `--metrics-bind-address` | `:8080` | Prometheus metrics address |

### Azure authentication

The controller uses `DefaultAzureCredential`, which tries (in order):
1. Azure Workload Identity (recommended for AKS)
2. Environment variables (`AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, `AZURE_TENANT_ID`)
3. Managed Identity
4. Azure CLI

## Cloud provider support

| Provider | Disk type | Status |
|----------|-----------|--------|
| Azure | Premium SSD v2 | Supported |
| GCP | Hyperdisk Balanced/Extreme | Not implemented |
| AWS | gp3/io2 | Not viable (6h modification cooldown) |

The CRD is cloud-agnostic. The controller infers the provider from the PV's CSI driver (`disk.csi.azure.com`, `pd.csi.storage.gke.io`).

## Planned work

- Prometheus metrics source (for sub-minute reaction times)
- Kubernetes events on scale actions
- Webhook validation for DiskAutoscalePolicy
