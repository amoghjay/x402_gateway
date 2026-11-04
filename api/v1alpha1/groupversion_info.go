// Package v1alpha1 is the x402.dev/v1alpha1 API: the PaymentPolicy an app team writes
// next to its HTTPRoute to sell that route per call.
//
// The CRD under deploy/k8s/crd and zz_generated.deepcopy.go are generated from the
// markers in this package by controller-gen (`make generate`); CI fails if they drift.
//
// +kubebuilder:object:generate=true
// +groupName=x402.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version this package serves.
var GroupVersion = schema.GroupVersion{Group: "x402.dev", Version: "v1alpha1"}

// SchemeBuilder registers the types below; AddToScheme is what a client's scheme calls.
// apimachinery's builder is enough here: the package needs nothing from controller-runtime.
var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &PaymentPolicy{}, &PaymentPolicyList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
