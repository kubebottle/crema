/*
Copyright 2026 kubebottle.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotRegistrySpec defines an OCI registry target for storing workspace rootfs snapshots.
type SnapshotRegistrySpec struct {
	// Repository is the target OCI repository prefix (e.g. "registry.crema-system.svc:5000/workspaces").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// SecretRef references a kubernetes.io/dockerconfigjson Secret for registry authentication.
	// +optional
	SecretRef *corev1.SecretReference `json:"secretRef,omitempty"`

	// PlainHTTP allows plain HTTP instead of HTTPS (for local development registries).
	// +kubebuilder:default=false
	// +optional
	PlainHTTP bool `json:"plainHTTP,omitempty"`

	// InsecureSkipTLSVerify skips TLS certificate verification when connecting over HTTPS.
	// +kubebuilder:default=false
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sreg
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="PlainHTTP",type=boolean,JSONPath=`.spec.plainHTTP`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SnapshotRegistry is the Schema for cluster-wide OCI snapshot registry configuration.
type SnapshotRegistry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SnapshotRegistrySpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SnapshotRegistryList contains a list of SnapshotRegistry.
type SnapshotRegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotRegistry `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SnapshotRegistry{}, &SnapshotRegistryList{})
}
