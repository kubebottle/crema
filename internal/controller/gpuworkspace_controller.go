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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cremav1alpha1 "github.com/kubebottle/crema/api/v1alpha1"
	"github.com/kubebottle/crema/internal/agentpb"
)

const (
	workspaceFinalizer        = "crema.kubebottle.io/finalizer"
	defaultRegistryName       = "default"
	condTypeReady             = "Ready"
	condTypeSnapshotReady     = "SnapshotReady"
	condTypeTemplateValid     = "TemplateValid"
	condTypeDegraded          = "Degraded"
	opTypeSuspend             = "Suspend"
	opStageCommitting         = "Committing"
	opStageCommitted          = "Committed"
	requeueAfterDeleteWait    = 1 * time.Second
	requeueAfterCommitFailure = 10 * time.Second
)

// GPUWorkspaceReconciler reconciles a GPUWorkspace object.
type GPUWorkspaceReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	AgentClient AgentClient
}

// +kubebuilder:rbac:groups=crema.kubebottle.io,resources=gpuworkspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crema.kubebottle.io,resources=gpuworkspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crema.kubebottle.io,resources=gpuworkspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=crema.kubebottle.io,resources=snapshotregistries,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile drives GPUWorkspace lifecycle transitions safely (commit -> persist -> delete -> recreate).
func (r *GPUWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ws cremav1alpha1.GPUWorkspace
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion and finalizer cleanup.
	if !ws.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ws)
	}
	if !controllerutil.ContainsFinalizer(&ws, workspaceFinalizer) {
		controllerutil.AddFinalizer(&ws, workspaceFinalizer)
		if err := r.Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate spec.template.spec.containers[0].
	if len(ws.Spec.Template.Spec.Containers) == 0 {
		setCondition(&ws, condTypeTemplateValid, metav1.ConditionFalse, "MissingContainer", "spec.template.spec.containers must contain at least one container")
		setCondition(&ws, condTypeReady, metav1.ConditionFalse, "InvalidTemplate", "spec.template.spec.containers is empty")
		_ = r.Status().Update(ctx, &ws)
		return ctrl.Result{}, nil
	}

	desiredBaseImage := ws.Spec.Template.Spec.Containers[0].Image
	if ws.Status.BaseImage == "" {
		ws.Status.BaseImage = desiredBaseImage
		ws.Status.CurrentImage = desiredBaseImage
	}
	if desiredBaseImage != ws.Status.BaseImage {
		setCondition(&ws, condTypeTemplateValid, metav1.ConditionFalse, "BaseImageImmutable",
			fmt.Sprintf("changing containers[0].image from %q to %q after initialization is not supported", ws.Status.BaseImage, desiredBaseImage))
		setCondition(&ws, condTypeReady, metav1.ConditionFalse, "InvalidTemplate", "base image cannot be modified after initialization")
		if err := r.Status().Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	setCondition(&ws, condTypeTemplateValid, metav1.ConditionTrue, "Valid", "workspace template is valid")

	// Ensure PVCs from VolumeClaimTemplates exist (retained across Suspend/Recreate).
	if err := r.ensurePVCs(ctx, &ws); err != nil {
		log.Error(err, "failed to ensure PVCs")
		setCondition(&ws, condTypeDegraded, metav1.ConditionTrue, "PVCError", err.Error())
		_ = r.Status().Update(ctx, &ws)
		return ctrl.Result{}, err
	}

	// Continue in-flight crash-recoverable operation if present.
	if ws.Status.Operation != nil {
		return r.reconcileOperation(ctx, &ws)
	}

	templateHash, err := ComputeTemplateHash(ws.Spec.Template)
	if err != nil {
		return ctrl.Result{}, err
	}

	pod, err := r.getWorkspacePod(ctx, &ws)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Suspend flow.
	if ws.Spec.Suspend {
		if pod == nil {
			ws.Status.Phase = cremav1alpha1.PhaseSuspended
			ws.Status.PodName = ""
			ws.Status.AppliedTemplateHash = templateHash
			ws.Status.ObservedGeneration = ws.Generation
			setCondition(&ws, condTypeReady, metav1.ConditionFalse, "Suspended", "workspace is suspended and GPU resources are released")
			if err := r.Status().Update(ctx, &ws); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		// If Pod has a started container, commit its rootfs before deleting.
		if containerID, ok := mainContainerID(pod); ok && pod.Spec.NodeName != "" {
			return r.startOperation(ctx, &ws, pod, containerID, opTypeSuspend, "Suspend")
		}

		// Pod never started a container yet; delete directly without committing.
		if err := r.deletePodWithUID(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		ws.Status.Phase = cremav1alpha1.PhaseSnapshotting
		_ = r.Status().Update(ctx, &ws)
		return ctrl.Result{RequeueAfter: requeueAfterDeleteWait}, nil
	}

	// Active (non-suspended) flow.
	if pod == nil {
		if ws.Status.Phase == cremav1alpha1.PhaseSuspended || ws.Status.Phase == cremav1alpha1.PhaseRecreating {
			ws.Status.Phase = cremav1alpha1.PhaseRecreating
		} else {
			ws.Status.Phase = cremav1alpha1.PhasePending
		}
		if err := r.createWorkspacePod(ctx, &ws, ws.Status.CurrentImage, templateHash); err != nil {
			setCondition(&ws, condTypeDegraded, metav1.ConditionTrue, "PodCreateFailed", err.Error())
			_ = r.Status().Update(ctx, &ws)
			return ctrl.Result{}, err
		}
		ws.Status.PodName = WorkspacePodName(ws.Name)
		ws.Status.AppliedTemplateHash = templateHash
		ws.Status.ObservedGeneration = ws.Generation
		setCondition(&ws, condTypeReady, metav1.ConditionFalse, "PodCreating", "waiting for workspace Pod to become Ready")
		setCondition(&ws, condTypeDegraded, metav1.ConditionFalse, "Healthy", "no errors")
		if err := r.Status().Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Track Pod node & baseImageDigest once scheduled/pulled.
	ws.Status.PodName = pod.Name
	if pod.Spec.NodeName != "" {
		ws.Status.NodeName = pod.Spec.NodeName
	}
	if ws.Status.BaseImageDigest == "" {
		if imgID := mainContainerImageID(pod); imgID != "" {
			ws.Status.BaseImageDigest = imgID
		}
	}

	// Strategy B: refuse template changes while workspace is active; require suspend=true first.
	if ws.Status.AppliedTemplateHash != "" && ws.Status.AppliedTemplateHash != templateHash {
		if _, ok := mainContainerID(pod); ok {
			setCondition(&ws, condTypeTemplateValid, metav1.ConditionFalse, "MustSuspendBeforeUpdate",
				"cannot modify spec.template while workspace is active; set spec.suspend=true first, wait for Suspended, then update template and resume")
			if err := r.Status().Update(ctx, &ws); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Pod hasn't started container yet; delete and recreate directly.
		if err := r.deletePodWithUID(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		ws.Status.Phase = cremav1alpha1.PhaseRecreating
		_ = r.Status().Update(ctx, &ws)
		return ctrl.Result{RequeueAfter: requeueAfterDeleteWait}, nil
	}

	if isPodReady(pod) {
		ws.Status.Phase = cremav1alpha1.PhaseRunning
		ws.Status.AppliedTemplateHash = templateHash
		ws.Status.ObservedGeneration = ws.Generation
		setCondition(&ws, condTypeReady, metav1.ConditionTrue, "PodReady", "workspace Pod is Ready")
		setCondition(&ws, condTypeDegraded, metav1.ConditionFalse, "Healthy", "workspace is healthy")
	} else {
		if ws.Status.Phase == "" {
			ws.Status.Phase = cremav1alpha1.PhasePending
		}
		setCondition(&ws, condTypeReady, metav1.ConditionFalse, "PodNotReady", fmt.Sprintf("Pod phase is %s", pod.Status.Phase))
	}

	if err := r.Status().Update(ctx, &ws); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *GPUWorkspaceReconciler) reconcileDelete(ctx context.Context, ws *cremav1alpha1.GPUWorkspace) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ws, workspaceFinalizer) {
		return ctrl.Result{}, nil
	}
	pod, err := r.getWorkspacePod(ctx, ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod != nil {
		if err := r.deletePodWithUID(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfterDeleteWait}, nil
	}
	controllerutil.RemoveFinalizer(ws, workspaceFinalizer)
	if err := r.Update(ctx, ws); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *GPUWorkspaceReconciler) startOperation(
	ctx context.Context,
	ws *cremav1alpha1.GPUWorkspace,
	pod *corev1.Pod,
	containerID, opType, reason string,
) (ctrl.Result, error) {
	sourceImage := ws.Status.CurrentImage
	if len(pod.Spec.Containers) > 0 && pod.Spec.Containers[0].Image != "" {
		sourceImage = pod.Spec.Containers[0].Image
	}
	ws.Status.Phase = cremav1alpha1.PhaseSnapshotting
	ws.Status.Operation = &cremav1alpha1.OperationStatus{
		ID:           uuid.NewString(),
		Type:         opType,
		Stage:        opStageCommitting,
		SourcePodUID: string(pod.UID),
		SourceImage:  sourceImage,
		ContainerID:  containerID,
		NodeName:     pod.Spec.NodeName,
		Generation:   ws.Generation,
		Reason:       reason,
		TargetSeq:    ws.Status.SnapshotSeq + 1,
	}
	setCondition(ws, condTypeReady, metav1.ConditionFalse, "Snapshotting", "capturing container rootfs delta")
	if err := r.Status().Update(ctx, ws); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *GPUWorkspaceReconciler) reconcileOperation(ctx context.Context, ws *cremav1alpha1.GPUWorkspace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	op := ws.Status.Operation

	pod, err := r.getWorkspacePod(ctx, ws)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch op.Stage {
	case opStageCommitting:
		// If source Pod disappeared before commit succeeded, recover cleanly.
		if pod == nil || string(pod.UID) != op.SourcePodUID {
			log.Info("source pod lost before commit completed, recovering from last currentImage", "operationID", op.ID)
			ws.Status.Operation = nil
			setCondition(ws, condTypeSnapshotReady, metav1.ConditionFalse, "SourcePodLost", "source Pod disappeared before snapshot completed")
			if ws.Spec.Suspend {
				ws.Status.Phase = cremav1alpha1.PhaseSuspended
				ws.Status.PodName = ""
			} else {
				ws.Status.Phase = cremav1alpha1.PhaseRecreating
			}
			if err := r.Status().Update(ctx, ws); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		resp, err := r.invokeAgentCommit(ctx, ws, pod, op)
		if err != nil {
			log.Error(err, "agent CommitContainer failed, retaining source Pod", "operationID", op.ID)
			ws.Status.Phase = cremav1alpha1.PhaseFailed
			setCondition(ws, condTypeSnapshotReady, metav1.ConditionFalse, "CommitFailed", err.Error())
			setCondition(ws, condTypeDegraded, metav1.ConditionTrue, "SnapshotFailed", err.Error())
			_ = r.Status().Update(ctx, ws)
			return ctrl.Result{RequeueAfter: requeueAfterCommitFailure}, nil
		}

		ws.Status.CurrentImage = resp.ImageRef
		ws.Status.SnapshotSeq = op.TargetSeq
		op.CommittedImage = resp.ImageRef
		op.Stage = opStageCommitted
		setCondition(ws, condTypeSnapshotReady, metav1.ConditionTrue, "Committed",
			fmt.Sprintf("committed %s (%s)", resp.ImageDigest, resp.DiffSummary))
		setCondition(ws, condTypeDegraded, metav1.ConditionFalse, "Healthy", "snapshot committed")
		if err := r.Status().Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case opStageCommitted:
		if pod != nil && string(pod.UID) == op.SourcePodUID {
			if err := r.deletePodWithUID(ctx, pod); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueAfterDeleteWait}, nil
		}

		// Source Pod is gone; finalize operation.
		templateHash, _ := ComputeTemplateHash(ws.Spec.Template)
		ws.Status.Operation = nil
		ws.Status.PodName = ""
		ws.Status.AppliedTemplateHash = templateHash
		ws.Status.ObservedGeneration = ws.Generation
		if ws.Spec.Suspend {
			ws.Status.Phase = cremav1alpha1.PhaseSuspended
			setCondition(ws, condTypeReady, metav1.ConditionFalse, "Suspended", "workspace suspended after snapshot")
			if err := r.Status().Update(ctx, ws); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		// User set suspend=false before deletion finished; recreate from CurrentImage.
		ws.Status.Phase = cremav1alpha1.PhaseRecreating
		if err := r.Status().Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	default:
		ws.Status.Operation = nil
		if err := r.Status().Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
}

func (r *GPUWorkspaceReconciler) invokeAgentCommit(
	ctx context.Context,
	ws *cremav1alpha1.GPUWorkspace,
	pod *corev1.Pod,
	op *cremav1alpha1.OperationStatus,
) (*agentpb.CommitContainerResponse, error) {
	if r.AgentClient == nil {
		return nil, fmt.Errorf("agent client is not configured")
	}

	regName := defaultRegistryName
	if ws.Spec.RegistryRef != nil && ws.Spec.RegistryRef.Name != "" {
		regName = ws.Spec.RegistryRef.Name
	}

	var sreg cremav1alpha1.SnapshotRegistry
	if err := r.Get(ctx, types.NamespacedName{Name: regName}, &sreg); err != nil {
		return nil, fmt.Errorf("get SnapshotRegistry %q: %w", regName, err)
	}

	var dockerConfigJSON []byte
	if sreg.Spec.SecretRef != nil && sreg.Spec.SecretRef.Name != "" {
		ns := sreg.Spec.SecretRef.Namespace
		if ns == "" {
			ns = ws.Namespace
		}
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: sreg.Spec.SecretRef.Name}, &sec); err != nil {
			return nil, fmt.Errorf("get registry secret %s/%s: %w", ns, sreg.Spec.SecretRef.Name, err)
		}
		dockerConfigJSON = sec.Data[corev1.DockerConfigJsonKey]
	}

	targetRepo := strings.TrimSuffix(sreg.Spec.Repository, "/") + "/" + ws.Namespace + "-" + ws.Name
	targetTag := fmt.Sprintf("rev-%d", op.TargetSeq)

	parentImage := op.SourceImage
	if parentImage == "" {
		parentImage = ws.Status.CurrentImage
	}

	return r.AgentClient.CommitContainer(ctx, op.NodeName, &agentpb.CommitContainerRequest{
		OperationId:           op.ID,
		WorkspaceUid:          string(ws.UID),
		WorkspaceNamespace:    ws.Namespace,
		WorkspaceName:         ws.Name,
		PodName:               pod.Name,
		PodUid:                string(pod.UID),
		ContainerName:         ws.Spec.Template.Spec.Containers[0].Name,
		ContainerId:           op.ContainerID,
		TargetRepository:      targetRepo,
		TargetTag:             targetTag,
		ParentImageRef:        parentImage,
		Compression:           "zstd",
		PlainHttp:             sreg.Spec.PlainHTTP,
		InsecureSkipTlsVerify: sreg.Spec.InsecureSkipTLSVerify,
		DockerConfigJson:      dockerConfigJSON,
		KeepQuiesced:          true,
	})
}

func (r *GPUWorkspaceReconciler) ensurePVCs(ctx context.Context, ws *cremav1alpha1.GPUWorkspace) error {
	for _, vct := range ws.Spec.VolumeClaimTemplates {
		pvcName := WorkspacePVCName(ws.Name, vct.Metadata.Name)
		var existing corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: pvcName}, &existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		labels := make(map[string]string, len(vct.Metadata.Labels)+1)
		for k, v := range vct.Metadata.Labels {
			labels[k] = v
		}
		labels[labelWorkspaceName] = ws.Name
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pvcName,
				Namespace:   ws.Namespace,
				Labels:      labels,
				Annotations: vct.Metadata.Annotations,
			},
			Spec: *vct.Spec.DeepCopy(),
		}
		if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func (r *GPUWorkspaceReconciler) getWorkspacePod(ctx context.Context, ws *cremav1alpha1.GPUWorkspace) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: WorkspacePodName(ws.Name)}, &pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &pod, nil
}

func (r *GPUWorkspaceReconciler) createWorkspacePod(ctx context.Context, ws *cremav1alpha1.GPUWorkspace, image, templateHash string) error {
	pod := BuildWorkspacePod(ws, image, templateHash)
	if err := controllerutil.SetControllerReference(ws, pod, r.Scheme); err != nil {
		return err
	}
	err := r.Create(ctx, pod)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (r *GPUWorkspaceReconciler) deletePodWithUID(ctx context.Context, pod *corev1.Pod) error {
	if !pod.DeletionTimestamp.IsZero() {
		return nil
	}
	err := r.Delete(ctx, pod, &client.DeleteOptions{
		Preconditions:      &metav1.Preconditions{UID: &pod.UID},
		GracePeriodSeconds: ptr.To(int64(2)),
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func mainContainerID(pod *corev1.Pod) (string, bool) {
	if len(pod.Status.ContainerStatuses) == 0 {
		return "", false
	}
	cid := pod.Status.ContainerStatuses[0].ContainerID
	if cid == "" {
		return "", false
	}
	return cid, true
}

func mainContainerImageID(pod *corev1.Pod) string {
	if len(pod.Status.ContainerStatuses) == 0 {
		return ""
	}
	imgID := pod.Status.ContainerStatuses[0].ImageID
	if strings.Contains(imgID, "@sha256:") {
		return imgID
	}
	return ""
}

func setCondition(ws *cremav1alpha1.GPUWorkspace, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ws.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *GPUWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cremav1alpha1.GPUWorkspace{}).
		Owns(&corev1.Pod{}).
		Named("gpuworkspace").
		Complete(r)
}
