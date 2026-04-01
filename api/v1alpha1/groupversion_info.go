// Package v1alpha1 contains API Schema definitions for the disk autoscaler v1alpha1 API group
// +kubebuilder:object:generate=true
// +groupName=disk.autoscaler.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "disk.autoscaler.io", Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	AddToScheme = SchemeBuilder.AddToScheme
)
