package registry

import (
	"testing"
	"time"

	v1alpha1 "github.com/padoa/vertical-disk-autoscaler/api/v1alpha1"
)

func TestRegisterAndGet(t *testing.T) {
	r := New()

	disk := &ManagedDisk{
		PVCName:      "data",
		PVCNamespace: "default",
		DiskID:       "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Compute/disks/disk1",
		PolicyName:   "policy1",
		FirstSeen:    time.Now(),
		Phase:        v1alpha1.DiskPhaseInitializing,
	}

	r.Register(disk)

	got := r.Get("default", "data")
	if got == nil {
		t.Fatal("expected disk, got nil")
	}
	if got.DiskID != disk.DiskID {
		t.Errorf("expected diskID %s, got %s", disk.DiskID, got.DiskID)
	}
}

func TestGetReturnsNilForUnknown(t *testing.T) {
	r := New()
	if got := r.Get("default", "nonexistent"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	r := New()
	r.Register(&ManagedDisk{PVCName: "data", PVCNamespace: "default", Phase: v1alpha1.DiskPhaseIdle})

	got := r.Get("default", "data")
	got.Phase = v1alpha1.DiskPhaseError

	original := r.Get("default", "data")
	if original.Phase != v1alpha1.DiskPhaseIdle {
		t.Error("Get should return a copy, but modifying it changed the registry")
	}
}

func TestUnregister(t *testing.T) {
	r := New()
	r.Register(&ManagedDisk{PVCName: "data", PVCNamespace: "default"})
	r.Unregister("default", "data")

	if got := r.Get("default", "data"); got != nil {
		t.Errorf("expected nil after unregister, got %+v", got)
	}
}

func TestList(t *testing.T) {
	r := New()
	r.Register(&ManagedDisk{PVCName: "a", PVCNamespace: "ns1"})
	r.Register(&ManagedDisk{PVCName: "b", PVCNamespace: "ns1"})
	r.Register(&ManagedDisk{PVCName: "c", PVCNamespace: "ns2"})

	list := r.List()
	if len(list) != 3 {
		t.Errorf("expected 3 disks, got %d", len(list))
	}
}

func TestListByPolicy(t *testing.T) {
	r := New()
	r.Register(&ManagedDisk{PVCName: "a", PVCNamespace: "ns1", PolicyName: "p1", PolicyNamespace: "ns1"})
	r.Register(&ManagedDisk{PVCName: "b", PVCNamespace: "ns1", PolicyName: "p1", PolicyNamespace: "ns1"})
	r.Register(&ManagedDisk{PVCName: "c", PVCNamespace: "ns1", PolicyName: "p2", PolicyNamespace: "ns1"})

	list := r.ListByPolicy("ns1", "p1")
	if len(list) != 2 {
		t.Errorf("expected 2 disks for policy p1, got %d", len(list))
	}

	list = r.ListByPolicy("ns1", "p2")
	if len(list) != 1 {
		t.Errorf("expected 1 disk for policy p2, got %d", len(list))
	}

	list = r.ListByPolicy("ns1", "nonexistent")
	if len(list) != 0 {
		t.Errorf("expected 0 disks for nonexistent policy, got %d", len(list))
	}
}

func TestReRegisterPreservesState(t *testing.T) {
	r := New()
	firstSeen := time.Now().Add(-10 * time.Minute)
	lastScale := time.Now().Add(-5 * time.Minute)

	r.Register(&ManagedDisk{
		PVCName:           "data",
		PVCNamespace:      "default",
		DiskID:            "disk1",
		FirstSeen:         firstSeen,
		CurrentIOPS:       20000,
		CurrentThroughputMBps: 400,
		LastScaleUp:       &lastScale,
		ScaleHistory:      []time.Time{lastScale},
		Phase:             v1alpha1.DiskPhaseCooldownUp,
	})

	// Re-register with updated config but same PVC.
	r.Register(&ManagedDisk{
		PVCName:      "data",
		PVCNamespace: "default",
		DiskID:       "disk1-new",
		PolicyName:   "new-policy",
		FirstSeen:    time.Now(), // Should be ignored.
	})

	got := r.Get("default", "data")
	if got.DiskID != "disk1-new" {
		t.Error("expected DiskID to be updated")
	}
	if got.PolicyName != "new-policy" {
		t.Error("expected PolicyName to be updated")
	}
	if !got.FirstSeen.Equal(firstSeen) {
		t.Error("FirstSeen should be preserved on re-registration")
	}
	if got.CurrentIOPS != 20000 {
		t.Errorf("CurrentIOPS should be preserved, got %d", got.CurrentIOPS)
	}
	if got.CurrentThroughputMBps != 400 {
		t.Errorf("CurrentThroughputMBps should be preserved, got %d", got.CurrentThroughputMBps)
	}
	if got.LastScaleUp == nil || !got.LastScaleUp.Equal(lastScale) {
		t.Error("LastScaleUp should be preserved")
	}
	if len(got.ScaleHistory) != 1 {
		t.Error("ScaleHistory should be preserved")
	}
	if got.Phase != v1alpha1.DiskPhaseCooldownUp {
		t.Errorf("Phase should be preserved, got %s", got.Phase)
	}
}

func TestUpdateAfterScale(t *testing.T) {
	r := New()
	r.Register(&ManagedDisk{
		PVCName:      "data",
		PVCNamespace: "default",
		CurrentIOPS:  10000,
		Phase:        v1alpha1.DiskPhaseIdle,
	})

	now := time.Now()
	r.UpdateAfterScale("default", "data", 15000, 300, v1alpha1.DiskPhaseScalingUp, now)

	got := r.Get("default", "data")
	if got.CurrentIOPS != 15000 {
		t.Errorf("expected IOPS 15000, got %d", got.CurrentIOPS)
	}
	if got.CurrentThroughputMBps != 300 {
		t.Errorf("expected throughput 300, got %d", got.CurrentThroughputMBps)
	}
	if got.LastScaleUp == nil || !got.LastScaleUp.Equal(now) {
		t.Error("LastScaleUp should be set")
	}
	if got.Phase != v1alpha1.DiskPhaseCooldownUp {
		t.Errorf("expected CooldownUp phase, got %s", got.Phase)
	}
	if len(got.ScaleHistory) != 1 {
		t.Errorf("expected 1 scale history entry, got %d", len(got.ScaleHistory))
	}
}

func TestUpdateAfterScale_PrunesOldHistory(t *testing.T) {
	r := New()
	r.Register(&ManagedDisk{
		PVCName:      "data",
		PVCNamespace: "default",
		ScaleHistory: []time.Time{
			time.Now().Add(-2 * time.Hour), // Old, should be pruned.
			time.Now().Add(-30 * time.Minute), // Recent, should stay.
		},
	})

	now := time.Now()
	r.UpdateAfterScale("default", "data", 5000, 200, v1alpha1.DiskPhaseScalingDown, now)

	got := r.Get("default", "data")
	// Old entry pruned, recent entry kept, new entry added = 2.
	if len(got.ScaleHistory) != 2 {
		t.Errorf("expected 2 scale history entries after pruning, got %d", len(got.ScaleHistory))
	}
}

func TestSetPhase(t *testing.T) {
	r := New()
	r.Register(&ManagedDisk{PVCName: "data", PVCNamespace: "default", Phase: v1alpha1.DiskPhaseInitializing})

	r.SetPhase("default", "data", v1alpha1.DiskPhaseIdle)

	got := r.Get("default", "data")
	if got.Phase != v1alpha1.DiskPhaseIdle {
		t.Errorf("expected Idle phase, got %s", got.Phase)
	}
}

func TestSetPhase_NonexistentDisk(t *testing.T) {
	r := New()
	// Should not panic.
	r.SetPhase("default", "nonexistent", v1alpha1.DiskPhaseError)
}

func TestUpdateAfterScale_NonexistentDisk(t *testing.T) {
	r := New()
	// Should not panic.
	r.UpdateAfterScale("default", "nonexistent", 5000, 200, v1alpha1.DiskPhaseScalingUp, time.Now())
}
