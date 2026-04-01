package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// DiskAutoscalePolicy defines an autoscaling policy for Azure Premium SSD v2 disks.
type DiskAutoscalePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DiskAutoscalePolicySpec   `json:"spec,omitempty"`
	Status DiskAutoscalePolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DiskAutoscalePolicyList contains a list of DiskAutoscalePolicy.
type DiskAutoscalePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DiskAutoscalePolicy `json:"items"`
}

type DiskAutoscalePolicySpec struct {
	// PVCSelector selects PVCs by label. If set, all matching PVCs are managed.
	// +optional
	PVCSelector *metav1.LabelSelector `json:"pvcSelector,omitempty"`

	// StorageClassName selects all PVCs using this StorageClass.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// ScaleUp configures upscaling behavior.
	ScaleUp ScaleDirectionConfig `json:"scaleUp"`

	// ScaleDown configures downscaling behavior.
	ScaleDown ScaleDirectionConfig `json:"scaleDown"`

	// Constraints defines the min/max bounds for IOPS and throughput.
	Constraints ScalingConstraints `json:"constraints"`

	// Behavior defines timing-related settings.
	// +optional
	Behavior BehaviorConfig `json:"behavior,omitempty"`

	// RateLimit defines per-disk scaling rate limits.
	// +optional
	RateLimit *RateLimitConfig `json:"rateLimit,omitempty"`
}

type ScaleDirectionConfig struct {
	// Enabled controls whether scaling in this direction is active.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// TargetIOPSUtilizationPercent is the target IOPS utilization that triggers scaling.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetIOPSUtilizationPercent int32 `json:"targetIOPSUtilizationPercent"`

	// TargetThroughputUtilizationPercent is the target throughput utilization that triggers scaling.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetThroughputUtilizationPercent int32 `json:"targetThroughputUtilizationPercent"`

	// StepIOPS is the number of IOPS to add/remove per scaling action.
	// +kubebuilder:validation:Minimum=1
	StepIOPS int64 `json:"stepIOPS"`

	// StepThroughputMBps is the throughput in MB/s to add/remove per scaling action.
	// +kubebuilder:validation:Minimum=1
	StepThroughputMBps int64 `json:"stepThroughputMBps"`

	// Cooldown is the minimum duration between two scaling actions in this direction.
	// +kubebuilder:default="5m"
	Cooldown metav1.Duration `json:"cooldown"`
}

type ScalingConstraints struct {
	// MinIOPS is the minimum IOPS the autoscaler will set.
	// +kubebuilder:validation:Minimum=3000
	// +kubebuilder:default=3000
	MinIOPS int64 `json:"minIOPS"`

	// MaxIOPS is the maximum IOPS the autoscaler will set.
	// +kubebuilder:validation:Minimum=3000
	MaxIOPS int64 `json:"maxIOPS"`

	// MinThroughputMBps is the minimum throughput in MB/s.
	// +kubebuilder:validation:Minimum=125
	// +kubebuilder:default=125
	MinThroughputMBps int64 `json:"minThroughputMBps"`

	// MaxThroughputMBps is the maximum throughput in MB/s.
	// +kubebuilder:validation:Minimum=125
	MaxThroughputMBps int64 `json:"maxThroughputMBps"`
}

type BehaviorConfig struct {
	// InitializationPeriod is the duration after a PVC is first seen during which no scaling occurs.
	// +kubebuilder:default="5m"
	// +optional
	InitializationPeriod metav1.Duration `json:"initializationPeriod,omitempty"`

	// MetricsWindow is the duration over which metrics are averaged for scaling decisions.
	// +kubebuilder:default="5m"
	// +optional
	MetricsWindow metav1.Duration `json:"metricsWindow,omitempty"`
}

type RateLimitConfig struct {
	// MaxScalesPerHour is the maximum number of scaling actions per disk per hour.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=6
	// +optional
	MaxScalesPerHour int32 `json:"maxScalesPerHour,omitempty"`
}

// DiskAutoscalePolicyStatus defines the observed state.
type DiskAutoscalePolicyStatus struct {
	// Disks contains the status of each managed disk.
	// +optional
	Disks []DiskStatus `json:"disks,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type DiskPhase string

const (
	DiskPhaseIdle         DiskPhase = "Idle"
	DiskPhaseScalingUp    DiskPhase = "ScalingUp"
	DiskPhaseScalingDown  DiskPhase = "ScalingDown"
	DiskPhaseCooldownUp   DiskPhase = "CooldownUp"
	DiskPhaseCooldownDown DiskPhase = "CooldownDown"
	DiskPhaseInitializing DiskPhase = "Initializing"
	DiskPhaseError        DiskPhase = "Error"
)

type DiskStatus struct {
	// PVCName is the name of the PVC.
	PVCName string `json:"pvcName"`
	// PVCNamespace is the namespace of the PVC.
	PVCNamespace string `json:"pvcNamespace"`
	// AzureDiskResourceID is the full Azure resource ID of the managed disk.
	AzureDiskResourceID string `json:"azureDiskResourceID"`

	// CurrentIOPS is the currently provisioned IOPS.
	CurrentIOPS int64 `json:"currentIOPS"`
	// CurrentThroughputMBps is the currently provisioned throughput in MB/s.
	CurrentThroughputMBps int64 `json:"currentThroughputMBps"`

	// LastScaleUp is the timestamp of the last scale-up action.
	// +optional
	LastScaleUp *metav1.Time `json:"lastScaleUp,omitempty"`
	// LastScaleDown is the timestamp of the last scale-down action.
	// +optional
	LastScaleDown *metav1.Time `json:"lastScaleDown,omitempty"`

	// Phase indicates the current state of this disk's autoscaling.
	Phase DiskPhase `json:"phase"`

	// LastMetrics contains the most recently observed utilization.
	// +optional
	LastMetrics *DiskMetricsSnapshot `json:"lastMetrics,omitempty"`

	// LastError contains the last error message if phase is Error.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

type DiskMetricsSnapshot struct {
	// IOPSUtilizationPercent is the observed IOPS utilization (0-100).
	IOPSUtilizationPercent int32 `json:"iopsUtilizationPercent"`
	// ThroughputUtilizationPercent is the observed throughput utilization (0-100).
	ThroughputUtilizationPercent int32 `json:"throughputUtilizationPercent"`
	// Timestamp is when these metrics were observed.
	Timestamp metav1.Time `json:"timestamp"`
}

func init() {
	SchemeBuilder.Register(&DiskAutoscalePolicy{}, &DiskAutoscalePolicyList{})
}
