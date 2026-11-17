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

// Phase represents the lifecycle phase of a GPUWorkspace.
// +kubebuilder:validation:Enum=Pending;Running;Snapshotting;Suspended;Recreating;Failed
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseRunning      Phase = "Running"
	PhaseSnapshotting Phase = "Snapshotting"
	PhaseSuspended    Phase = "Suspended"
	PhaseRecreating   Phase = "Recreating"
	PhaseFailed       Phase = "Failed"
)

// VolumeClaimMetadata defines metadata for an operator-managed PVC template.
type VolumeClaimMetadata struct {
	// Name of the volume claim template; the created PVC is named <workspace>-<name>.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// VolumeClaimTemplate defines a PVC created and retained by the workspace lifecycle.
type VolumeClaimTemplate struct {
	Metadata VolumeClaimMetadata              `json:"metadata"`
	Spec     corev1.PersistentVolumeClaimSpec `json:"spec"`
}

// GPUWorkspaceSpec defines the desired state of GPUWorkspace.
type GPUWorkspaceSpec struct {
	// Template defines the Pod used to run the workspace (LWS / AIBrix pattern).
	// Rootfs snapshotting and restoration always target template.spec.containers[0].
	// +kubebuilder:validation:Required
	Template corev1.PodTemplateSpec `json:"template"`

	// VolumeClaimTemplates optionally defines PVCs created and retained by the
	// workspace lifecycle (following StatefulSet / LWS conventions).
	// +optional
	VolumeClaimTemplates []VolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`

	// Suspend requests a snapshot-safe release of the workspace Pod to free GPUs and compute.
	// On snapshot failure, the Pod is retained rather than entering Suspended.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// RegistryRef references a cluster-scoped SnapshotRegistry by name.
	// Defaults to "default" when omitted.
	// +optional
	RegistryRef *corev1.LocalObjectReference `json:"registryRef,omitempty"`
}

// OperationStatus persists an in-flight snapshot/recreate/suspend operation before
// mutating external state (agent commit or Pod deletion), enabling crash-safe retries.
type OperationStatus struct {
	ID                 string       `json:"id"`
	Type               string       `json:"type"`  // Recreate | Suspend
	Stage              string       `json:"stage"` // Committing | Committed | Recreating
	SourcePodUID       string       `json:"sourcePodUID,omitempty"`
	ContainerID        string       `json:"containerID,omitempty"`
	NodeName           string       `json:"nodeName,omitempty"`
	Generation         int64        `json:"generation,omitempty"`
	Reason             string       `json:"reason,omitempty"`
	TargetSeq          int64        `json:"targetSeq,omitempty"`
	CommittedImage     string       `json:"committedImage,omitempty"`
	QuiescenceDeadline *metav1.Time `json:"quiescenceDeadline,omitempty"`
}

// GPUWorkspaceStatus defines the observed state of GPUWorkspace.
type GPUWorkspaceStatus struct {
	// Phase is the current high-level lifecycle phase.
	Phase Phase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the workspace state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent Spec generation processed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BaseImage is the initial base image recorded from spec.template.spec.containers[0].image.
	BaseImage string `json:"baseImage,omitempty"`

	// BaseImageDigest is the resolved immutable reference for the initial base image.
	BaseImageDigest string `json:"baseImageDigest,omitempty"`

	// CurrentImage is the active immutable OCI image reference (base image or latest snapshot).
	CurrentImage string `json:"currentImage,omitempty"`

	// SnapshotSeq is a monotonic counter used to generate unique OCI tags (rev-1, rev-2, ...).
	SnapshotSeq int64 `json:"snapshotSeq,omitempty"`

	// AppliedTemplateHash is the hash of spec.template currently running or last applied.
	AppliedTemplateHash string `json:"appliedTemplateHash,omitempty"`

	// PodName is the name of the currently active workspace Pod.
	PodName string `json:"podName,omitempty"`

	// NodeName is the node hosting the current (or last) workspace Pod, used for soft affinity.
	NodeName string `json:"nodeName,omitempty"`

	// Operation tracks an in-flight snapshot/recreate/suspend operation for crash recovery.
	Operation *OperationStatus `json:"operation,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gws
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Seq",type=integer,JSONPath=`.status.snapshotSeq`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GPUWorkspace is the Schema for the gpuworkspaces API.
type GPUWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUWorkspaceSpec   `json:"spec,omitempty"`
	Status GPUWorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUWorkspaceList contains a list of GPUWorkspace.
type GPUWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUWorkspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUWorkspace{}, &GPUWorkspaceList{})
}
