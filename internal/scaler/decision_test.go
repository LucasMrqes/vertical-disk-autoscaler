package scaler

import (
	"testing"
	"time"

	v1alpha1 "github.com/padoa/vertical-disk-autoscaler/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func defaultSpec() v1alpha1.DiskAutoscalePolicySpec {
	return v1alpha1.DiskAutoscalePolicySpec{
		ScaleUp: v1alpha1.ScaleDirectionConfig{
			Enabled:                            true,
			TargetIOPSUtilizationPercent:       80,
			TargetThroughputUtilizationPercent: 80,
			StepIOPS:                           5000,
			StepThroughputMBps:                 100,
			Cooldown:                           metav1.Duration{Duration: 2 * time.Minute},
		},
		ScaleDown: v1alpha1.ScaleDirectionConfig{
			Enabled:                            true,
			TargetIOPSUtilizationPercent:       30,
			TargetThroughputUtilizationPercent: 30,
			StepIOPS:                           2000,
			StepThroughputMBps:                 50,
			Cooldown:                           metav1.Duration{Duration: 10 * time.Minute},
		},
		Constraints: v1alpha1.ScalingConstraints{
			MinIOPS:           3000,
			MaxIOPS:           40000,
			MinThroughputMBps: 125,
			MaxThroughputMBps: 600,
		},
		Behavior: v1alpha1.BehaviorConfig{
			InitializationPeriod: metav1.Duration{Duration: 5 * time.Minute},
			MetricsWindow:        metav1.Duration{Duration: 5 * time.Minute},
		},
	}
}

func TestEvaluate_InitializationPeriod(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  3000,
		CurrentThroughputMBps:        125,
		IOPSUtilizationPercent:       95,
		ThroughputUtilizationPercent: 95,
		FirstSeen:                    now.Add(-2 * time.Minute), // Only 2 min ago.
	}

	action := Evaluate(state, defaultSpec(), now)
	if action != nil {
		t.Errorf("expected no action during initialization period, got %+v", action)
	}
}

func TestEvaluate_ScaleUp_HighIOPS(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  10000,
		CurrentThroughputMBps:        200,
		IOPSUtilizationPercent:       90,
		ThroughputUtilizationPercent: 50,
		FirstSeen:                    now.Add(-10 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action == nil {
		t.Fatal("expected scale up action")
	}
	if action.Direction != v1alpha1.DiskPhaseScalingUp {
		t.Errorf("expected ScalingUp, got %s", action.Direction)
	}
	if action.NewIOPS != 15000 {
		t.Errorf("expected IOPS 15000, got %d", action.NewIOPS)
	}
	// Throughput unchanged (below target).
	if action.NewThroughputMBps != 200 {
		t.Errorf("expected throughput 200, got %d", action.NewThroughputMBps)
	}
}

func TestEvaluate_ScaleUp_HighThroughput(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  20000,
		CurrentThroughputMBps:        300,
		IOPSUtilizationPercent:       50,
		ThroughputUtilizationPercent: 90,
		FirstSeen:                    now.Add(-10 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action == nil {
		t.Fatal("expected scale up action")
	}
	if action.NewThroughputMBps != 400 {
		t.Errorf("expected throughput 400, got %d", action.NewThroughputMBps)
	}
	// IOPS unchanged (below target).
	if action.NewIOPS != 20000 {
		t.Errorf("expected IOPS 20000, got %d", action.NewIOPS)
	}
}

func TestEvaluate_ScaleDown_BothLow(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  20000,
		CurrentThroughputMBps:        400,
		IOPSUtilizationPercent:       10,
		ThroughputUtilizationPercent: 15,
		FirstSeen:                    now.Add(-30 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action == nil {
		t.Fatal("expected scale down action")
	}
	if action.Direction != v1alpha1.DiskPhaseScalingDown {
		t.Errorf("expected ScalingDown, got %s", action.Direction)
	}
	if action.NewIOPS != 18000 {
		t.Errorf("expected IOPS 18000, got %d", action.NewIOPS)
	}
	if action.NewThroughputMBps != 350 {
		t.Errorf("expected throughput 350, got %d", action.NewThroughputMBps)
	}
}

func TestEvaluate_ScaleDown_OnlyOneLow(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  20000,
		CurrentThroughputMBps:        400,
		IOPSUtilizationPercent:       10, // Below target.
		ThroughputUtilizationPercent: 50, // Above target.
		FirstSeen:                    now.Add(-30 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action != nil {
		t.Errorf("expected no action when only one dimension is low, got %+v", action)
	}
}

func TestEvaluate_Cooldown_ScaleUp(t *testing.T) {
	now := time.Now()
	lastScale := now.Add(-1 * time.Minute) // 1 min ago, cooldown is 2 min.
	state := DiskState{
		CurrentIOPS:                  10000,
		CurrentThroughputMBps:        200,
		IOPSUtilizationPercent:       95,
		ThroughputUtilizationPercent: 95,
		FirstSeen:                    now.Add(-30 * time.Minute),
		LastScaleUp:                  &lastScale,
	}

	action := Evaluate(state, defaultSpec(), now)
	if action != nil {
		t.Errorf("expected no action during cooldown, got %+v", action)
	}
}

func TestEvaluate_ClampToMax(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  38000,
		CurrentThroughputMBps:        550,
		IOPSUtilizationPercent:       95,
		ThroughputUtilizationPercent: 95,
		FirstSeen:                    now.Add(-30 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action == nil {
		t.Fatal("expected scale up action")
	}
	if action.NewIOPS != 40000 {
		t.Errorf("expected IOPS clamped to 40000, got %d", action.NewIOPS)
	}
	if action.NewThroughputMBps != 600 {
		t.Errorf("expected throughput clamped to 600, got %d", action.NewThroughputMBps)
	}
}

func TestEvaluate_ClampToMin(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  4000,
		CurrentThroughputMBps:        150,
		IOPSUtilizationPercent:       5,
		ThroughputUtilizationPercent: 5,
		FirstSeen:                    now.Add(-30 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action == nil {
		t.Fatal("expected scale down action")
	}
	if action.NewIOPS != 3000 {
		t.Errorf("expected IOPS clamped to 3000, got %d", action.NewIOPS)
	}
	if action.NewThroughputMBps != 125 {
		t.Errorf("expected throughput clamped to 125, got %d", action.NewThroughputMBps)
	}
}

func TestEvaluate_AlreadyAtMax(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  40000,
		CurrentThroughputMBps:        600,
		IOPSUtilizationPercent:       95,
		ThroughputUtilizationPercent: 95,
		FirstSeen:                    now.Add(-30 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action != nil {
		t.Errorf("expected no action when already at max, got %+v", action)
	}
}

func TestEvaluate_AlreadyAtMin(t *testing.T) {
	now := time.Now()
	state := DiskState{
		CurrentIOPS:                  3000,
		CurrentThroughputMBps:        125,
		IOPSUtilizationPercent:       5,
		ThroughputUtilizationPercent: 5,
		FirstSeen:                    now.Add(-30 * time.Minute),
	}

	action := Evaluate(state, defaultSpec(), now)
	if action != nil {
		t.Errorf("expected no action when already at min, got %+v", action)
	}
}

func TestEvaluate_RateLimitExceeded(t *testing.T) {
	now := time.Now()
	spec := defaultSpec()
	spec.RateLimit = &v1alpha1.RateLimitConfig{MaxScalesPerHour: 2}

	state := DiskState{
		CurrentIOPS:                  10000,
		CurrentThroughputMBps:        200,
		IOPSUtilizationPercent:       95,
		ThroughputUtilizationPercent: 95,
		FirstSeen:                    now.Add(-30 * time.Minute),
		ScalesInLastHour:             2,
	}

	action := Evaluate(state, spec, now)
	if action != nil {
		t.Errorf("expected no action when rate limit exceeded, got %+v", action)
	}
}

func TestEnforceThroughputIOPSCoupling(t *testing.T) {
	constraints := v1alpha1.ScalingConstraints{
		MinIOPS: 3000, MaxIOPS: 40000,
		MinThroughputMBps: 125, MaxThroughputMBps: 600,
	}

	// Case: throughput within IOPS capacity.
	iops, tp := enforceThroughputIOPSCoupling(20000, 400, constraints)
	if iops != 20000 || tp != 400 {
		t.Errorf("expected no change, got IOPS=%d throughput=%d", iops, tp)
	}

	// Case: throughput exceeds IOPS capacity, IOPS can be bumped.
	iops, tp = enforceThroughputIOPSCoupling(1000, 500, constraints)
	if iops != 2000 || tp != 500 {
		t.Errorf("expected IOPS=2000 throughput=500, got IOPS=%d throughput=%d", iops, tp)
	}

	// Case: throughput requires IOPS above max — cap both.
	iops, tp = enforceThroughputIOPSCoupling(1000, 20000, constraints)
	if iops != 40000 || tp != 10000 {
		t.Errorf("expected IOPS=40000 throughput=10000, got IOPS=%d throughput=%d", iops, tp)
	}
}
