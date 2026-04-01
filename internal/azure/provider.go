package azure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/monitor/armmonitor"
	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
)

const (
	CSIDriverName = "disk.csi.azure.com"

	// Azure Monitor metric names for managed disks.
	metricReadOps  = "Composite Disk Read Operations/sec"
	metricWriteOps = "Composite Disk Write Operations/sec"
	metricReadBps  = "Composite Disk Read Bytes/sec"
	metricWriteBps = "Composite Disk Write Bytes/sec"
)

// DiskPerformance holds the provisioned performance of a disk.
type DiskPerformance struct {
	IOPS          int64
	ThroughputMBps int64
}

// DiskMetrics holds observed utilization.
type DiskMetrics struct {
	IOPSUtilizationPercent       float64
	ThroughputUtilizationPercent float64
	Timestamp                    time.Time
}

// Provider implements disk operations for Azure Premium SSD v2.
type Provider struct {
	log         logr.Logger
	limiter     *rate.Limiter
	diskClients map[string]*armcompute.DisksClient       // keyed by subscription ID
	metricClients map[string]*armmonitor.MetricsClient   // keyed by subscription ID
	cred        azcore.TokenCredential
}

// NewProvider creates a new Azure provider with the given credential and global rate limiter.
func NewProvider(log logr.Logger, cred azcore.TokenCredential, maxWritesPerHour int) *Provider {
	// Convert writes/hour to a rate.Limiter: e.g. 600/hr = 10/min = 1 every 6s.
	r := rate.Every(time.Hour / time.Duration(maxWritesPerHour))
	return &Provider{
		log:           log,
		limiter:       rate.NewLimiter(r, 5), // burst of 5
		diskClients:   make(map[string]*armcompute.DisksClient),
		metricClients: make(map[string]*armmonitor.MetricsClient),
		cred:          cred,
	}
}

// ResolveDiskID extracts the Azure disk resource ID from a PersistentVolume.
// Returns empty string if the PV is not an Azure CSI disk.
func (p *Provider) ResolveDiskID(pv *corev1.PersistentVolume) string {
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != CSIDriverName {
		return ""
	}
	return pv.Spec.CSI.VolumeHandle
}

// GetDiskPerformance reads the current provisioned IOPS and throughput of an Azure disk.
func (p *Provider) GetDiskPerformance(ctx context.Context, diskResourceID string) (*DiskPerformance, error) {
	subID, rg, diskName, err := parseDiskResourceID(diskResourceID)
	if err != nil {
		return nil, err
	}

	client, err := p.getDiskClient(subID)
	if err != nil {
		return nil, fmt.Errorf("creating disk client: %w", err)
	}

	resp, err := client.Get(ctx, rg, diskName, nil)
	if err != nil {
		return nil, fmt.Errorf("getting disk %s: %w", diskName, err)
	}

	var iops, throughput int64
	if resp.Properties != nil {
		if resp.Properties.DiskIOPSReadWrite != nil {
			iops = *resp.Properties.DiskIOPSReadWrite
		}
		if resp.Properties.DiskMBpsReadWrite != nil {
			throughput = *resp.Properties.DiskMBpsReadWrite
		}
	}

	return &DiskPerformance{
		IOPS:           iops,
		ThroughputMBps: throughput,
	}, nil
}

// SetDiskPerformance updates the provisioned IOPS and throughput of an Azure disk.
func (p *Provider) SetDiskPerformance(ctx context.Context, diskResourceID string, perf DiskPerformance) error {
	if err := p.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit: %w", err)
	}

	subID, rg, diskName, err := parseDiskResourceID(diskResourceID)
	if err != nil {
		return err
	}

	client, err := p.getDiskClient(subID)
	if err != nil {
		return fmt.Errorf("creating disk client: %w", err)
	}

	p.log.Info("updating disk performance",
		"disk", diskName,
		"resourceGroup", rg,
		"newIOPS", perf.IOPS,
		"newThroughputMBps", perf.ThroughputMBps,
	)

	update := armcompute.DiskUpdate{
		Properties: &armcompute.DiskUpdateProperties{
			DiskIOPSReadWrite: &perf.IOPS,
			DiskMBpsReadWrite: &perf.ThroughputMBps,
		},
	}

	poller, err := client.BeginUpdate(ctx, rg, diskName, update, nil)
	if err != nil {
		return fmt.Errorf("starting disk update: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("waiting for disk update: %w", err)
	}

	return nil
}

// GetDiskMetrics fetches IOPS and throughput utilization from Azure Monitor.
func (p *Provider) GetDiskMetrics(ctx context.Context, diskResourceID string, window time.Duration, provisionedIOPS, provisionedThroughputMBps int64) (*DiskMetrics, error) {
	subID, _, _, err := parseDiskResourceID(diskResourceID)
	if err != nil {
		return nil, err
	}

	client, err := p.getMetricsClient(subID)
	if err != nil {
		return nil, fmt.Errorf("creating metrics client: %w", err)
	}

	now := time.Now().UTC()
	start := now.Add(-window)
	timespan := fmt.Sprintf("%s/%s", start.Format(time.RFC3339), now.Format(time.RFC3339))
	metricNames := fmt.Sprintf("%s,%s,%s,%s", metricReadOps, metricWriteOps, metricReadBps, metricWriteBps)
	interval := "PT1M"
	aggregation := "Average"

	resp, err := client.List(ctx, diskResourceID, &armmonitor.MetricsClientListOptions{
		Timespan:    &timespan,
		Interval:    &interval,
		Metricnames: &metricNames,
		Aggregation: &aggregation,
	})
	if err != nil {
		return nil, fmt.Errorf("querying metrics: %w", err)
	}

	var totalReadOps, totalWriteOps, totalReadBps, totalWriteBps float64
	var sampleCount int

	for _, metric := range resp.Value {
		if metric.Name == nil || metric.Name.Value == nil || metric.Timeseries == nil {
			continue
		}
		for _, ts := range metric.Timeseries {
			for _, dp := range ts.Data {
				if dp.Average == nil {
					continue
				}
				sampleCount++
				switch *metric.Name.Value {
				case metricReadOps:
					totalReadOps += *dp.Average
				case metricWriteOps:
					totalWriteOps += *dp.Average
				case metricReadBps:
					totalReadBps += *dp.Average
				case metricWriteBps:
					totalWriteBps += *dp.Average
				}
			}
		}
	}

	if sampleCount == 0 {
		return &DiskMetrics{
			Timestamp: now,
		}, nil
	}

	// Each metric type contributes sampleCount/4 data points.
	perMetricSamples := float64(sampleCount / 4)
	if perMetricSamples == 0 {
		perMetricSamples = 1
	}

	avgIOPS := (totalReadOps + totalWriteOps) / perMetricSamples
	avgThroughputBps := (totalReadBps + totalWriteBps) / perMetricSamples
	avgThroughputMBps := avgThroughputBps / (1024 * 1024)

	var iopsUtil, throughputUtil float64
	if provisionedIOPS > 0 {
		iopsUtil = (avgIOPS / float64(provisionedIOPS)) * 100
	}
	if provisionedThroughputMBps > 0 {
		throughputUtil = (avgThroughputMBps / float64(provisionedThroughputMBps)) * 100
	}

	return &DiskMetrics{
		IOPSUtilizationPercent:       iopsUtil,
		ThroughputUtilizationPercent: throughputUtil,
		Timestamp:                    now,
	}, nil
}

func (p *Provider) getDiskClient(subscriptionID string) (*armcompute.DisksClient, error) {
	if c, ok := p.diskClients[subscriptionID]; ok {
		return c, nil
	}
	c, err := armcompute.NewDisksClient(subscriptionID, p.cred, &arm.ClientOptions{})
	if err != nil {
		return nil, err
	}
	p.diskClients[subscriptionID] = c
	return c, nil
}

func (p *Provider) getMetricsClient(subscriptionID string) (*armmonitor.MetricsClient, error) {
	if c, ok := p.metricClients[subscriptionID]; ok {
		return c, nil
	}
	c, err := armmonitor.NewMetricsClient(subscriptionID, p.cred, nil)
	if err != nil {
		return nil, err
	}
	p.metricClients[subscriptionID] = c
	return c, nil
}

// parseDiskResourceID extracts subscription, resource group, and disk name from an Azure resource ID.
// Expected format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Compute/disks/{name}
func parseDiskResourceID(resourceID string) (subscriptionID, resourceGroup, diskName string, err error) {
	parts := strings.Split(strings.TrimPrefix(resourceID, "/"), "/")
	if len(parts) < 8 {
		return "", "", "", fmt.Errorf("invalid Azure disk resource ID: %s", resourceID)
	}

	lookup := make(map[string]string)
	for i := 0; i+1 < len(parts); i += 2 {
		lookup[strings.ToLower(parts[i])] = parts[i+1]
	}

	subscriptionID = lookup["subscriptions"]
	resourceGroup = lookup["resourcegroups"]
	diskName = lookup["disks"]

	if subscriptionID == "" || resourceGroup == "" || diskName == "" {
		return "", "", "", fmt.Errorf("could not parse Azure disk resource ID: %s", resourceID)
	}

	return subscriptionID, resourceGroup, diskName, nil
}
