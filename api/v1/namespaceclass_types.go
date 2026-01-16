package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ResourceTemplate represents one item in NamespaceClass.spec.resources
type ResourceTemplate struct {
	// Template is the K8s resource object (any GVK)
	// +kubebuilder:pruning:PreserveUnknownFields
	Template runtime.RawExtension `json:"template"`
}

// DeletionPolicy controls behavior when a NamespaceClass is deleted
type DeletionPolicy string

const (
	DeletionPolicyCascade DeletionPolicy = "Cascade"
	DeletionPolicyOrphan  DeletionPolicy = "Orphan"
)

// NamespaceClassSpec defines the desired state of NamespaceClass
type NamespaceClassSpec struct {
	// Resources is a list of resource templates to be created in the target namespace.
	Resources []ResourceTemplate `json:"resources,omitempty"`
	// DeletionPolicy determines behavior when this NamespaceClass is deleted.
	// Accepted values: Cascade (default) or Orphan.
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// NamespaceClassStatus defines the observed state of NamespaceClass
type NamespaceClassStatus struct {
	SyncedNamespaces []string    `json:"syncedNamespaces,omitempty"`
	LastSyncTime     metav1.Time `json:"lastSyncTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// NamespaceClass is the Schema for the namespaceclasses API
type NamespaceClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NamespaceClassSpec   `json:"spec,omitempty"`
	Status NamespaceClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NamespaceClassList contains a list of NamespaceClass
type NamespaceClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NamespaceClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NamespaceClass{}, &NamespaceClassList{})
}
