package scaler

import (
	"fmt"
	"time"

	v1alpha1 "github.com/padoa/vertical-disk-autoscaler/api/v1alpha1"
)

// DiskState represents the current state of a disk for scaling decisions.
type DiskState struct {
	CurrentIOPS           int64
	CurrentThroughputMBps int64

	// Observed utilization (0-100).
	IOPSUtilizationPercent       float64
	ThroughputUtilizationPercent float64

	LastScaleUp   *time.Time
	LastScaleDown *time.Time
	FirstSeen     time.Time

	// Number of scale events in the last hour.
	ScalesInLastHour int
}

// ScaleAction describes a scaling action to take.
type ScaleAction struct {
	NewIOPS           int64
	NewThroughputMBps int64
	Direction         v1alpha1.DiskPhase
	Reason            string
}

// Evaluate determines if a scaling action is needed. Returns nil if no action is needed.
func Evaluate(state DiskState, spec v1alpha1.DiskAutoscalePolicySpec, now time.Time) *ScaleAction {
	// Check initialization period.
	if now.Before(state.FirstSeen.Add(spec.Behavior.InitializationPeriod.Duration)) {
		return nil
	}

	// Check per-disk rate limit.
	if spec.RateLimit != nil && spec.RateLimit.MaxScalesPerHour > 0 {
		if state.ScalesInLastHour >= int(spec.RateLimit.MaxScalesPerHour) {
			return nil
		}
	}

	// Evaluate scale-up: use MAX of both utilization dimensions.
	if spec.ScaleUp.Enabled {
		if action := evaluateScaleUp(state, spec, now); action != nil {
			return action
		}
	}

	// Evaluate scale-down: use MIN of both utilization dimensions.
	if spec.ScaleDown.Enabled {
		if action := evaluateScaleDown(state, spec, now); action != nil {
			return action
		}
	}

	return nil
}

func evaluateScaleUp(state DiskState, spec v1alpha1.DiskAutoscalePolicySpec, now time.Time) *ScaleAction {
	cfg := spec.ScaleUp

	// Check cooldown.
	if state.LastScaleUp != nil && now.Before(state.LastScaleUp.Add(cfg.Cooldown.Duration)) {
		return nil
	}

	iopsOverTarget := state.IOPSUtilizationPercent > float64(cfg.TargetIOPSUtilizationPercent)
	throughputOverTarget := state.ThroughputUtilizationPercent > float64(cfg.TargetThroughputUtilizationPercent)

	if !iopsOverTarget && !throughputOverTarget {
		return nil
	}

	newIOPS := state.CurrentIOPS
	newThroughput := state.CurrentThroughputMBps

	if iopsOverTarget {
		newIOPS = state.CurrentIOPS + cfg.StepIOPS
	}
	if throughputOverTarget {
		newThroughput = state.CurrentThroughputMBps + cfg.StepThroughputMBps
	}

	// Clamp to constraints.
	newIOPS = clamp(newIOPS, spec.Constraints.MinIOPS, spec.Constraints.MaxIOPS)
	newThroughput = clamp(newThroughput, spec.Constraints.MinThroughputMBps, spec.Constraints.MaxThroughputMBps)

	// Enforce Azure throughput-to-IOPS coupling: throughput <= IOPS * 0.25
	newIOPS, newThroughput = enforceThroughputIOPSCoupling(newIOPS, newThroughput, spec.Constraints)

	// No change needed — already at max.
	if newIOPS == state.CurrentIOPS && newThroughput == state.CurrentThroughputMBps {
		return nil
	}

	reason := fmt.Sprintf("IOPS utilization %.1f%% (target %d%%), throughput utilization %.1f%% (target %d%%)",
		state.IOPSUtilizationPercent, cfg.TargetIOPSUtilizationPercent,
		state.ThroughputUtilizationPercent, cfg.TargetThroughputUtilizationPercent)

	return &ScaleAction{
		NewIOPS:           newIOPS,
		NewThroughputMBps: newThroughput,
		Direction:         v1alpha1.DiskPhaseScalingUp,
		Reason:            "scale up: " + reason,
	}
}

func evaluateScaleDown(state DiskState, spec v1alpha1.DiskAutoscalePolicySpec, now time.Time) *ScaleAction {
	cfg := spec.ScaleDown

	// Check cooldown.
	if state.LastScaleDown != nil && now.Before(state.LastScaleDown.Add(cfg.Cooldown.Duration)) {
		return nil
	}

	iopsUnderTarget := state.IOPSUtilizationPercent < float64(cfg.TargetIOPSUtilizationPercent)
	throughputUnderTarget := state.ThroughputUtilizationPercent < float64(cfg.TargetThroughputUtilizationPercent)

	// Both must be under target to scale down (conservative).
	if !iopsUnderTarget || !throughputUnderTarget {
		return nil
	}

	newIOPS := state.CurrentIOPS - cfg.StepIOPS
	newThroughput := state.CurrentThroughputMBps - cfg.StepThroughputMBps

	// Clamp to constraints.
	newIOPS = clamp(newIOPS, spec.Constraints.MinIOPS, spec.Constraints.MaxIOPS)
	newThroughput = clamp(newThroughput, spec.Constraints.MinThroughputMBps, spec.Constraints.MaxThroughputMBps)

	// Enforce Azure throughput-to-IOPS coupling.
	newIOPS, newThroughput = enforceThroughputIOPSCoupling(newIOPS, newThroughput, spec.Constraints)

	// No change needed — already at min.
	if newIOPS == state.CurrentIOPS && newThroughput == state.CurrentThroughputMBps {
		return nil
	}

	reason := fmt.Sprintf("IOPS utilization %.1f%% (target %d%%), throughput utilization %.1f%% (target %d%%)",
		state.IOPSUtilizationPercent, cfg.TargetIOPSUtilizationPercent,
		state.ThroughputUtilizationPercent, cfg.TargetThroughputUtilizationPercent)

	return &ScaleAction{
		NewIOPS:           newIOPS,
		NewThroughputMBps: newThroughput,
		Direction:         v1alpha1.DiskPhaseScalingDown,
		Reason:            "scale down: " + reason,
	}
}

// enforceThroughputIOPSCoupling ensures throughput <= IOPS * 0.25 (Azure Premium SSD v2 constraint).
// If throughput is too high for the IOPS, bump IOPS up. If that exceeds maxIOPS, cap throughput down.
func enforceThroughputIOPSCoupling(iops, throughputMBps int64, constraints v1alpha1.ScalingConstraints) (int64, int64) {
	maxThroughputForIOPS := iops / 4 // 0.25 MB/s per IOPS
	if throughputMBps <= maxThroughputForIOPS {
		return iops, throughputMBps
	}

	// Need more IOPS to support this throughput.
	requiredIOPS := throughputMBps * 4
	if requiredIOPS <= constraints.MaxIOPS {
		return requiredIOPS, throughputMBps
	}

	// Can't get enough IOPS — cap throughput instead.
	return constraints.MaxIOPS, constraints.MaxIOPS / 4
}

func clamp(value, min, max int64) int64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
