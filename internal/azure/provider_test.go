package azure

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseDiskResourceID(t *testing.T) {
	tests := []struct {
		name       string
		resourceID string
		wantSub    string
		wantRG     string
		wantDisk   string
		wantErr    bool
	}{
		{
			name:       "valid resource ID",
			resourceID: "/subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Compute/disks/pvc-abc",
			wantSub:    "sub-123",
			wantRG:     "my-rg",
			wantDisk:   "pvc-abc",
		},
		{
			name:       "case insensitive keys",
			resourceID: "/Subscriptions/sub-123/ResourceGroups/my-rg/Providers/Microsoft.Compute/Disks/pvc-abc",
			wantSub:    "sub-123",
			wantRG:     "my-rg",
			wantDisk:   "pvc-abc",
		},
		{
			name:       "too short",
			resourceID: "/subscriptions/sub-123",
			wantErr:    true,
		},
		{
			name:       "empty string",
			resourceID: "",
			wantErr:    true,
		},
		{
			name:       "missing disk segment",
			resourceID: "/subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Compute",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, rg, disk, err := parseDiskResourceID(tt.resourceID)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sub != tt.wantSub {
				t.Errorf("subscription: got %q, want %q", sub, tt.wantSub)
			}
			if rg != tt.wantRG {
				t.Errorf("resource group: got %q, want %q", rg, tt.wantRG)
			}
			if disk != tt.wantDisk {
				t.Errorf("disk name: got %q, want %q", disk, tt.wantDisk)
			}
		})
	}
}

func TestResolveDiskID(t *testing.T) {
	p := &Provider{}

	tests := []struct {
		name   string
		pv     *corev1.PersistentVolume
		wantID string
	}{
		{
			name: "Azure CSI disk",
			pv: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:       "disk.csi.azure.com",
							VolumeHandle: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/disks/disk1",
						},
					},
				},
			},
			wantID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/disks/disk1",
		},
		{
			name: "GCP CSI disk — not Azure",
			pv: &corev1.PersistentVolume{
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:       "pd.csi.storage.gke.io",
							VolumeHandle: "projects/proj/zones/zone/disks/disk1",
						},
					},
				},
			},
			wantID: "",
		},
		{
			name: "no CSI source",
			pv: &corev1.PersistentVolume{
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/data"},
					},
				},
			},
			wantID: "",
		},
		{
			name: "nil CSI",
			pv: &corev1.PersistentVolume{
				Spec: corev1.PersistentVolumeSpec{},
			},
			wantID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.ResolveDiskID(tt.pv)
			if got != tt.wantID {
				t.Errorf("got %q, want %q", got, tt.wantID)
			}
		})
	}
}
