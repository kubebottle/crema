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
// +kubebuilder:validation:Enum=Pending;Running;Snapshotting;Paused;Recreating;Failed
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseRunning      Phase = "Running"
	PhaseSnapshotting Phase = "Snapshotting"
	PhasePaused       Phase = "Paused"
	PhaseRecreating   Phase = "Recreating"
	PhaseFailed       Phase = "Failed"
)

// SnapshotPolicy configures how workspace rootfs deltas are captured and retained.
type SnapshotPolicy struct {
	// RetentionLimit is the maximum number of snapshot records retained in status.
	// Protected revisions (such as currentRevision and active operation target) are never pruned.
	// Underlying blob reclamation depends on the OCI registry's GC policy.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	RetentionLimit int32 `json:"retentionLimit,omitempty"`

	// Compression specifies the OCI delta layer compression algorithm.
	// +kubebuilder:validation:Enum=zstd;gzip
	// +kubebuilder:default=zstd
	// +optional
	Compression string `json:"compression,omitempty"`

	// SnapshotTrigger triggers an on-demand snapshot whenever its value changes.
	// +optional
	SnapshotTrigger string `json:"snapshotTrigger,omitempty"`

	// Message attaches an optional human-readable note to the next snapshot.
	// +optional
	Message string `json:"message,omitempty"`
}

// RestoreRequest represents a one-shot request to roll back the workspace rootfs
// to a specific revision. Rollback only restores the container rootfs; it does
// not revert PVC contents, running processes, or GPU memory, and keeps the
// current spec.template compute configuration.
type RestoreRequest struct {
	// RequestID uniquely identifies this restore operation (e.g. UUID or timestamp).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	RequestID string `json:"requestID"`

	// Revision is the target snapshot revision (0 = initial base image).
	// +kubebuilder:validation:Minimum=0
	Revision int64 `json:"revision"`
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
	VolumeClaimTemplates []corev1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`

	// Paused requests a snapshot-safe release of the workspace Pod to free GPUs and compute.
	// On snapshot failure, the Pod is retained rather than entering Paused.
	// +kubebuilder:default=false
	// +optional
	Paused bool `json:"paused,omitempty"`

	// RegistryRef references a cluster-scoped SnapshotRegistry by name.
	// Defaults to "default" when omitted.
	// +optional
	RegistryRef *corev1.LocalObjectReference `json:"registryRef,omitempty"`

	// SnapshotPolicy configures snapshot compression, retention, and manual triggers.
	// +optional
	SnapshotPolicy SnapshotPolicy `json:"snapshotPolicy,omitempty"`

	// Restore requests a one-shot rollback of the workspace rootfs to a prior revision.
	// +optional
	Restore *RestoreRequest `json:"restore,omitempty"`
}

// OperationStatus persists an in-flight snapshot/recreate/restore operation before
// mutating external state (agent commit or Pod deletion), enabling crash-safe retries.
type OperationStatus struct {
	ID                 string       `json:"id"`
	Type               string       `json:"type"`  // Snapshot | Recreate | Pause | Restore
	Stage              string       `json:"stage"` // Committing | Committed | Recreating
	SourcePodUID       string       `json:"sourcePodUID,omitempty"`
	ContainerID        string       `json:"containerID,omitempty"`
	NodeName           string       `json:"nodeName,omitempty"`
	Generation         int64        `json:"generation,omitempty"`
	Reason             string       `json:"reason,omitempty"`
	TargetRevision     *int64       `json:"targetRevision,omitempty"`
	CommittedImage     string       `json:"committedImage,omitempty"`
	QuiescenceDeadline *metav1.Time `json:"quiescenceDeadline,omitempty"`
}

// SnapshotRecord records an immutable OCI snapshot revision of the workspace rootfs.
type SnapshotRecord struct {
	// Revision is the monotonically allocated snapshot number (1, 2, ...).
	Revision int64 `json:"revision"`

	// ParentRevision records the revision from which this delta branched (0 or nil = base image).
	// +optional
	ParentRevision *int64 `json:"parentRevision,omitempty"`

	// Image is the immutable OCI image reference (repo/ns-name:rev-N@sha256:...).
	Image string `json:"image"`

	// LayerDigest is the digest of the incremental delta layer produced in this revision.
	LayerDigest string `json:"layerDigest"`

	// LayerSizeBytes is the compressed size of the incremental delta layer in bytes.
	LayerSizeBytes int64 `json:"layerSizeBytes"`

	// Reason describes what triggered this snapshot (e.g. TemplateChange, Pause, Manual).
	Reason string `json:"reason"`

	// Summary is a compact diff summary extracted from the delta tar headers.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Message is an optional user-provided note for this snapshot.
	// +optional
	Message string `json:"message,omitempty"`

	// CreatedAt is the timestamp when the snapshot was committed.
	CreatedAt metav1.Time `json:"createdAt"`
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

	// BaseImageDigest is the resolved immutable reference for revision 0.
	BaseImageDigest string `json:"baseImageDigest,omitempty"`

	// CurrentImage is the active OCI image reference (base image at rev 0, or a snapshot).
	CurrentImage string `json:"currentImage,omitempty"`

	// CurrentRevision is the currently active revision number (0 = base image).
	CurrentRevision int64 `json:"currentRevision,omitempty"`

	// LastAllocatedRevision is the monotonic high-watermark counter for snapshot revisions.
	LastAllocatedRevision int64 `json:"lastAllocatedRevision,omitempty"`

	// AppliedTemplateHash is the hash of spec.template currently running or last applied.
	AppliedTemplateHash string `json:"appliedTemplateHash,omitempty"`

	// ObservedRestoreRequestID tracks the last processed spec.restore.requestID.
	ObservedRestoreRequestID string `json:"observedRestoreRequestID,omitempty"`

	// LastSnapshotTrigger tracks the last processed spec.snapshotPolicy.snapshotTrigger.
	LastSnapshotTrigger string `json:"lastSnapshotTrigger,omitempty"`

	// PodName is the name of the currently active workspace Pod.
	PodName string `json:"podName,omitempty"`

	// NodeName is the node hosting the current (or last) workspace Pod, used for soft affinity.
	NodeName string `json:"nodeName,omitempty"`

	// Operation tracks an in-flight snapshot/recreate/restore operation for crash recovery.
	Operation *OperationStatus `json:"operation,omitempty"`

	// Snapshots holds the retained snapshot revision history.
	Snapshots []SnapshotRecord `json:"snapshots,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gws
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Rev",type=integer,JSONPath=`.status.currentRevision`
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
