package registry

import (
	"sync"
	"time"

	v1alpha1 "github.com/padoa/vertical-disk-autoscaler/api/v1alpha1"
)

// ManagedDisk represents a disk that is being managed by the autoscaler.
type ManagedDisk struct {
	PVCName      string
	PVCNamespace string
	PVName       string
	DiskID       string // Azure resource ID or GCP disk path
	CSIDriver    string // e.g. "disk.csi.azure.com", "pd.csi.storage.gke.io"

	// PolicyRef points to the policy governing this disk.
	PolicyName      string
	PolicyNamespace string

	// FirstSeen is when this disk was first registered for autoscaling.
	FirstSeen time.Time

	// ScaleHistory tracks recent scaling actions for rate limiting.
	ScaleHistory []time.Time

	// LastScaleUp is the time of the last upscale.
	LastScaleUp *time.Time
	// LastScaleDown is the time of the last downscale.
	LastScaleDown *time.Time

	// CurrentIOPS is the last known provisioned IOPS.
	CurrentIOPS int64
	// CurrentThroughputMBps is the last known provisioned throughput.
	CurrentThroughputMBps int64

	// Phase is the current autoscaling phase.
	Phase v1alpha1.DiskPhase
}

// Registry is a thread-safe store of managed disks.
type Registry struct {
	mu    sync.RWMutex
	disks map[string]*ManagedDisk // keyed by "namespace/pvcName"
}

func New() *Registry {
	return &Registry{
		disks: make(map[string]*ManagedDisk),
	}
}

func diskKey(namespace, name string) string {
	return namespace + "/" + name
}

// Register adds or updates a managed disk.
func (r *Registry) Register(disk *ManagedDisk) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := diskKey(disk.PVCNamespace, disk.PVCName)
	if existing, ok := r.disks[key]; ok {
		// Preserve state that shouldn't be overwritten by re-registration.
		disk.FirstSeen = existing.FirstSeen
		disk.ScaleHistory = existing.ScaleHistory
		disk.LastScaleUp = existing.LastScaleUp
		disk.LastScaleDown = existing.LastScaleDown
		disk.CurrentIOPS = existing.CurrentIOPS
		disk.CurrentThroughputMBps = existing.CurrentThroughputMBps
		disk.Phase = existing.Phase
	}
	r.disks[key] = disk
}

// Unregister removes a managed disk.
func (r *Registry) Unregister(namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.disks, diskKey(namespace, name))
}

// Get returns a copy of a managed disk, or nil if not found.
func (r *Registry) Get(namespace, name string) *ManagedDisk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.disks[diskKey(namespace, name)]
	if !ok {
		return nil
	}
	cp := *d
	return &cp
}

// List returns copies of all managed disks.
func (r *Registry) List() []ManagedDisk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]ManagedDisk, 0, len(r.disks))
	for _, d := range r.disks {
		cp := *d
		result = append(result, cp)
	}
	return result
}

// UpdateAfterScale records a scaling event on a disk.
func (r *Registry) UpdateAfterScale(namespace, name string, newIOPS, newThroughputMBps int64, direction v1alpha1.DiskPhase, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	d, ok := r.disks[diskKey(namespace, name)]
	if !ok {
		return
	}

	d.CurrentIOPS = newIOPS
	d.CurrentThroughputMBps = newThroughputMBps
	d.ScaleHistory = append(d.ScaleHistory, now)

	switch direction {
	case v1alpha1.DiskPhaseScalingUp:
		d.LastScaleUp = &now
		d.Phase = v1alpha1.DiskPhaseCooldownUp
	case v1alpha1.DiskPhaseScalingDown:
		d.LastScaleDown = &now
		d.Phase = v1alpha1.DiskPhaseCooldownDown
	}

	// Prune scale history older than 1 hour.
	cutoff := now.Add(-time.Hour)
	pruned := d.ScaleHistory[:0]
	for _, t := range d.ScaleHistory {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	d.ScaleHistory = pruned
}

// SetPhase updates the phase of a managed disk.
func (r *Registry) SetPhase(namespace, name string, phase v1alpha1.DiskPhase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.disks[diskKey(namespace, name)]; ok {
		d.Phase = phase
	}
}

// SetError marks a disk as errored.
func (r *Registry) SetError(namespace, name string, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.disks[diskKey(namespace, name)]; ok {
		d.Phase = v1alpha1.DiskPhaseError
	}
}

// ListByPolicy returns all disks managed by a given policy.
func (r *Registry) ListByPolicy(policyNamespace, policyName string) []ManagedDisk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []ManagedDisk
	for _, d := range r.disks {
		if d.PolicyNamespace == policyNamespace && d.PolicyName == policyName {
			cp := *d
			result = append(result, cp)
		}
	}
	return result
}
