package controller

import (
	"context"
	"math"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/padoa/vertical-disk-autoscaler/api/v1alpha1"
	"github.com/padoa/vertical-disk-autoscaler/internal/azure"
	"github.com/padoa/vertical-disk-autoscaler/internal/registry"
	"github.com/padoa/vertical-disk-autoscaler/internal/scaler"
)

// ScalingLoop periodically evaluates all managed disks and applies scaling actions.
// It implements manager.Runnable.
type ScalingLoop struct {
	client   client.Client
	log      logr.Logger
	registry *registry.Registry
	provider *azure.Provider
	interval time.Duration
}

func NewScalingLoop(c client.Client, log logr.Logger, reg *registry.Registry, provider *azure.Provider, interval time.Duration) *ScalingLoop {
	return &ScalingLoop{
		client:   c,
		log:      log.WithName("scaling-loop"),
		registry: reg,
		provider: provider,
		interval: interval,
	}
}

// Start implements manager.Runnable.
func (s *ScalingLoop) Start(ctx context.Context) error {
	s.log.Info("starting scaling loop", "interval", s.interval)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("stopping scaling loop")
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *ScalingLoop) tick(ctx context.Context) {
	disks := s.registry.List()
	if len(disks) == 0 {
		return
	}

	s.log.V(1).Info("scaling tick", "managedDisks", len(disks))

	for i := range disks {
		if err := s.evaluateDisk(ctx, disks[i]); err != nil {
			s.log.Error(err, "failed to evaluate disk",
				"pvc", disks[i].PVCNamespace+"/"+disks[i].PVCName,
				"diskID", disks[i].DiskID,
			)
			s.registry.SetError(disks[i].PVCNamespace, disks[i].PVCName, err.Error())
		}
	}
}

func (s *ScalingLoop) evaluateDisk(ctx context.Context, disk registry.ManagedDisk) error {
	log := s.log.WithValues("pvc", disk.PVCNamespace+"/"+disk.PVCName, "diskID", disk.DiskID)

	// Fetch the policy for this disk.
	policy, err := s.getPolicy(ctx, disk.PolicyNamespace, disk.PolicyName)
	if err != nil {
		return err
	}

	// Fetch current disk performance if we don't have it yet.
	if disk.CurrentIOPS == 0 {
		perf, err := s.provider.GetDiskPerformance(ctx, disk.DiskID)
		if err != nil {
			return err
		}
		disk.CurrentIOPS = perf.IOPS
		disk.CurrentThroughputMBps = perf.ThroughputMBps
		s.registry.UpdateAfterScale(disk.PVCNamespace, disk.PVCName, perf.IOPS, perf.ThroughputMBps, v1alpha1.DiskPhaseIdle, time.Now())
	}

	// Fetch metrics from Azure Monitor.
	metricsWindow := policy.Spec.Behavior.MetricsWindow.Duration
	if metricsWindow == 0 {
		metricsWindow = 5 * time.Minute
	}

	metrics, err := s.provider.GetDiskMetrics(ctx, disk.DiskID, metricsWindow, disk.CurrentIOPS, disk.CurrentThroughputMBps)
	if err != nil {
		return err
	}

	now := time.Now()

	// Build state for the decision function.
	state := scaler.DiskState{
		CurrentIOPS:                  disk.CurrentIOPS,
		CurrentThroughputMBps:        disk.CurrentThroughputMBps,
		IOPSUtilizationPercent:       metrics.IOPSUtilizationPercent,
		ThroughputUtilizationPercent: metrics.ThroughputUtilizationPercent,
		LastScaleUp:                  disk.LastScaleUp,
		LastScaleDown:                disk.LastScaleDown,
		FirstSeen:                    disk.FirstSeen,
		ScalesInLastHour:             len(disk.ScaleHistory),
	}

	action := scaler.Evaluate(state, policy.Spec, now)

	// Update status with latest metrics regardless of action.
	s.updateDiskStatus(ctx, policy, disk, metrics, now)

	if action == nil {
		// No scaling needed — update phase to idle if past initialization.
		if now.After(disk.FirstSeen.Add(policy.Spec.Behavior.InitializationPeriod.Duration)) {
			if disk.Phase == v1alpha1.DiskPhaseInitializing {
				s.registry.SetPhase(disk.PVCNamespace, disk.PVCName, v1alpha1.DiskPhaseIdle)
			}
		}
		return nil
	}

	log.Info("scaling disk",
		"direction", action.Direction,
		"reason", action.Reason,
		"currentIOPS", disk.CurrentIOPS,
		"newIOPS", action.NewIOPS,
		"currentThroughputMBps", disk.CurrentThroughputMBps,
		"newThroughputMBps", action.NewThroughputMBps,
	)

	// Apply the scaling action.
	s.registry.SetPhase(disk.PVCNamespace, disk.PVCName, action.Direction)

	err = s.provider.SetDiskPerformance(ctx, disk.DiskID, azure.DiskPerformance{
		IOPS:           action.NewIOPS,
		ThroughputMBps: action.NewThroughputMBps,
	})
	if err != nil {
		return err
	}

	s.registry.UpdateAfterScale(disk.PVCNamespace, disk.PVCName, action.NewIOPS, action.NewThroughputMBps, action.Direction, now)

	return nil
}

func (s *ScalingLoop) getPolicy(ctx context.Context, namespace, name string) (*v1alpha1.DiskAutoscalePolicy, error) {
	var policy v1alpha1.DiskAutoscalePolicy
	err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &policy)
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

func (s *ScalingLoop) updateDiskStatus(ctx context.Context, policy *v1alpha1.DiskAutoscalePolicy, disk registry.ManagedDisk, metrics *azure.DiskMetrics, now time.Time) {
	// Find or create the disk status entry.
	var found bool
	for i, ds := range policy.Status.Disks {
		if ds.PVCName == disk.PVCName && ds.PVCNamespace == disk.PVCNamespace {
			policy.Status.Disks[i].CurrentIOPS = disk.CurrentIOPS
			policy.Status.Disks[i].CurrentThroughputMBps = disk.CurrentThroughputMBps
			policy.Status.Disks[i].Phase = disk.Phase
			policy.Status.Disks[i].LastMetrics = &v1alpha1.DiskMetricsSnapshot{
				IOPSUtilizationPercent:       int32(math.Round(metrics.IOPSUtilizationPercent)),
				ThroughputUtilizationPercent: int32(math.Round(metrics.ThroughputUtilizationPercent)),
				Timestamp:                    metav1.NewTime(now),
			}
			if disk.LastScaleUp != nil {
				t := metav1.NewTime(*disk.LastScaleUp)
				policy.Status.Disks[i].LastScaleUp = &t
			}
			if disk.LastScaleDown != nil {
				t := metav1.NewTime(*disk.LastScaleDown)
				policy.Status.Disks[i].LastScaleDown = &t
			}
			found = true
			break
		}
	}

	if !found {
		status := v1alpha1.DiskStatus{
			PVCName:               disk.PVCName,
			PVCNamespace:          disk.PVCNamespace,
			AzureDiskResourceID:   disk.DiskID,
			CurrentIOPS:           disk.CurrentIOPS,
			CurrentThroughputMBps: disk.CurrentThroughputMBps,
			Phase:                 disk.Phase,
			LastMetrics: &v1alpha1.DiskMetricsSnapshot{
				IOPSUtilizationPercent:       int32(math.Round(metrics.IOPSUtilizationPercent)),
				ThroughputUtilizationPercent: int32(math.Round(metrics.ThroughputUtilizationPercent)),
				Timestamp:                    metav1.NewTime(now),
			},
		}
		policy.Status.Disks = append(policy.Status.Disks, status)
	}

	if err := s.client.Status().Update(ctx, policy); err != nil {
		s.log.Error(err, "failed to update policy status",
			"policy", policy.Namespace+"/"+policy.Name,
		)
	}
}
