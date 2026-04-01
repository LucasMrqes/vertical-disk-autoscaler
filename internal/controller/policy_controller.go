package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/padoa/vertical-disk-autoscaler/api/v1alpha1"
	"github.com/padoa/vertical-disk-autoscaler/internal/azure"
	"github.com/padoa/vertical-disk-autoscaler/internal/registry"
)

// PolicyController watches DiskAutoscalePolicies and PVCs,
// resolving matching PVCs to Azure disk IDs and registering them.
type PolicyController struct {
	client   client.Client
	log      logr.Logger
	registry *registry.Registry
	provider *azure.Provider
}

func NewPolicyController(c client.Client, log logr.Logger, reg *registry.Registry, provider *azure.Provider) *PolicyController {
	return &PolicyController{
		client:   c,
		log:      log.WithName("policy-controller"),
		registry: reg,
		provider: provider,
	}
}

func (r *PolicyController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("policy", req.NamespacedName)

	var policy v1alpha1.DiskAutoscalePolicy
	if err := r.client.Get(ctx, req.NamespacedName, &policy); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Policy deleted — unregister all disks for this policy.
			for _, disk := range r.registry.ListByPolicy(req.Namespace, req.Name) {
				r.registry.Unregister(disk.PVCNamespace, disk.PVCName)
				log.Info("unregistered disk after policy deletion", "pvc", disk.PVCNamespace+"/"+disk.PVCName)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Find all PVCs matching this policy.
	matchingPVCs, err := r.findMatchingPVCs(ctx, &policy)
	if err != nil {
		log.Error(err, "failed to list matching PVCs")
		return ctrl.Result{}, err
	}

	// Track which PVCs are still matched so we can unregister stale ones.
	currentKeys := make(map[string]bool)

	for _, pvc := range matchingPVCs {
		pvcKey := pvc.Namespace + "/" + pvc.Name
		currentKeys[pvcKey] = true

		// PVC must be bound.
		if pvc.Status.Phase != corev1.ClaimBound {
			continue
		}

		pvName := pvc.Spec.VolumeName
		if pvName == "" {
			continue
		}

		// Resolve PV -> Azure disk ID.
		var pv corev1.PersistentVolume
		if err := r.client.Get(ctx, types.NamespacedName{Name: pvName}, &pv); err != nil {
			log.Error(err, "failed to get PV", "pv", pvName)
			continue
		}

		diskID := r.provider.ResolveDiskID(&pv)
		if diskID == "" {
			log.V(1).Info("PV is not an Azure CSI disk, skipping", "pv", pvName)
			continue
		}

		disk := &registry.ManagedDisk{
			PVCName:         pvc.Name,
			PVCNamespace:    pvc.Namespace,
			PVName:          pvName,
			DiskID:          diskID,
			CSIDriver:       pv.Spec.CSI.Driver,
			PolicyName:      policy.Name,
			PolicyNamespace: policy.Namespace,
			FirstSeen:       time.Now(),
			Phase:           v1alpha1.DiskPhaseInitializing,
		}

		r.registry.Register(disk)
		log.Info("registered disk", "pvc", pvcKey, "diskID", diskID)
	}

	// Unregister disks that no longer match this policy.
	for _, disk := range r.registry.ListByPolicy(policy.Namespace, policy.Name) {
		key := disk.PVCNamespace + "/" + disk.PVCName
		if !currentKeys[key] {
			r.registry.Unregister(disk.PVCNamespace, disk.PVCName)
			log.Info("unregistered disk (no longer matches policy)", "pvc", key)
		}
	}

	return ctrl.Result{}, nil
}

func (r *PolicyController) findMatchingPVCs(ctx context.Context, policy *v1alpha1.DiskAutoscalePolicy) ([]corev1.PersistentVolumeClaim, error) {
	var allPVCs corev1.PersistentVolumeClaimList
	if err := r.client.List(ctx, &allPVCs); err != nil {
		return nil, err
	}

	var matched []corev1.PersistentVolumeClaim

	// Build label selector if specified.
	var labelSelector labels.Selector
	if policy.Spec.PVCSelector != nil {
		var err error
		labelSelector, err = convertLabelSelector(policy.Spec.PVCSelector)
		if err != nil {
			return nil, err
		}
	}

	for _, pvc := range allPVCs.Items {
		matches := false

		// Check label selector match.
		if labelSelector != nil && labelSelector.Matches(labels.Set(pvc.Labels)) {
			matches = true
		}

		// Check storage class match.
		if policy.Spec.StorageClassName != nil && pvc.Spec.StorageClassName != nil {
			if *pvc.Spec.StorageClassName == *policy.Spec.StorageClassName {
				matches = true
			}
		}

		if matches {
			matched = append(matched, pvc)
		}
	}

	return matched, nil
}

func convertLabelSelector(selector *metav1.LabelSelector) (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(selector)
}

func (r *PolicyController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DiskAutoscalePolicy{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.mapPVCToPolicy)).
		Complete(r)
}

// mapPVCToPolicy finds all policies that could match a given PVC and enqueues them for reconciliation.
func (r *PolicyController) mapPVCToPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	var policies v1alpha1.DiskAutoscalePolicyList
	if err := r.client.List(ctx, &policies); err != nil {
		r.log.Error(err, "failed to list policies for PVC mapping")
		return nil
	}

	var requests []reconcile.Request
	for _, policy := range policies.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      policy.Name,
				Namespace: policy.Namespace,
			},
		})
	}
	return requests
}
