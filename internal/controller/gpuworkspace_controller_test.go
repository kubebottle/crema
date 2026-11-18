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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cremav1alpha1 "github.com/kubebottle/crema/api/v1alpha1"
	"github.com/kubebottle/crema/internal/agentpb"
)

type mockAgentClient struct {
	commitErr   error
	commitCalls int
	lastReq     *agentpb.CommitContainerRequest
}

func (m *mockAgentClient) CommitContainer(_ context.Context, _ string, req *agentpb.CommitContainerRequest) (*agentpb.CommitContainerResponse, error) {
	m.commitCalls++
	m.lastReq = req
	if m.commitErr != nil {
		return nil, m.commitErr
	}
	digest := fmt.Sprintf("sha256:%064d", m.commitCalls)
	return &agentpb.CommitContainerResponse{
		ImageRef:       fmt.Sprintf("%s@%s", req.TargetRepository, digest),
		ImageDigest:    digest,
		LayerDigest:    "sha256:layer123",
		LayerSizeBytes: 4096,
		DiffSummary:    "~2 (root/.bashrc, usr/bin/jq)",
	}, nil
}

func runReconcileLoop(ctx context.Context, r *GPUWorkspaceReconciler, key types.NamespacedName, steps int) {
	for i := 0; i < steps; i++ {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}
}

func markPodRunningAndReady(ctx context.Context, key types.NamespacedName, nodeName, containerID, imageID string) {
	var pod corev1.Pod
	Expect(k8sClient.Get(ctx, key, &pod)).To(Succeed())

	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		Target:     corev1.ObjectReference{Kind: "Node", Name: nodeName},
	}
	Expect(k8sClient.SubResource("binding").Create(ctx, &pod, binding)).To(Succeed())

	Expect(k8sClient.Get(ctx, key, &pod)).To(Succeed())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name:        pod.Spec.Containers[0].Name,
			ContainerID: containerID,
			ImageID:     imageID,
			Ready:       true,
		},
	}
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
}

var _ = Describe("GPUWorkspace Controller", func() {
	ctx := context.Background()

	BeforeEach(func() {
		sreg := &cremav1alpha1.SnapshotRegistry{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, sreg)
		if errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, &cremav1alpha1.SnapshotRegistry{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: cremav1alpha1.SnapshotRegistrySpec{
					Repository: "registry.crema-system.svc:5000/workspaces",
					PlainHTTP:  true,
				},
			})).To(Succeed())
		}
	})

	It("enforces suspend-before-resize (Strategy B), retains Pod on commit failure, and restores with soft affinity", func() {
		wsName := "test-gws-lifecycle"
		wsKey := types.NamespacedName{Name: wsName, Namespace: "default"}
		podKey := types.NamespacedName{Name: WorkspacePodName(wsName), Namespace: "default"}

		ws := &cremav1alpha1.GPUWorkspace{
			ObjectMeta: metav1.ObjectMeta{Name: wsName, Namespace: "default"},
			Spec: cremav1alpha1.GPUWorkspaceSpec{
				VolumeClaimTemplates: []cremav1alpha1.VolumeClaimTemplate{
					{
						Metadata: cremav1alpha1.VolumeClaimMetadata{Name: "data"},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("10Gi"),
								},
							},
						},
					},
				},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:    "workspace",
								Image:   "ubuntu:22.04",
								Command: []string{"sleep", "infinity"},
								Resources: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										"nvidia.com/gpu": resource.MustParse("1"),
									},
								},
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, ws)).To(Succeed())
		defer func() {
			var latest cremav1alpha1.GPUWorkspace
			if err := k8sClient.Get(ctx, wsKey, &latest); err == nil {
				_ = k8sClient.Delete(ctx, &latest)
			}
		}()

		agent := &mockAgentClient{}
		r := &GPUWorkspaceReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			AgentClient: agent,
		}

		// 1. Add finalizer, create PVC & Pod -> Running on worker-1.
		runReconcileLoop(ctx, r, wsKey, 2)

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: WorkspacePVCName(wsName, "data"), Namespace: "default"}, &pvc)).To(Succeed())

		markPodRunningAndReady(ctx, podKey, "worker-1", "containerd://cid-1", "docker-pullable://ubuntu@sha256:base")
		runReconcileLoop(ctx, r, wsKey, 1)

		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		Expect(ws.Status.Phase).To(Equal(cremav1alpha1.PhaseRunning))
		Expect(ws.Status.NodeName).To(Equal("worker-1"))
		Expect(ws.Status.BaseImageDigest).To(ContainSubstring("@sha256:base"))

		// 2. Strategy B: modifying GPU while Running (suspend=false) must be rejected without touching the Pod.
		ws.Spec.Template.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"] = resource.MustParse("2")
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		runReconcileLoop(ctx, r, wsKey, 1)

		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		cond := meta.FindStatusCondition(ws.Status.Conditions, condTypeTemplateValid)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("MustSuspendBeforeUpdate"))
		Expect(agent.commitCalls).To(Equal(0))

		// 3. User sets suspend=true; simulate commit failure first -> Pod must be retained!
		agent.commitErr = fmt.Errorf("registry unreachable")
		ws.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		runReconcileLoop(ctx, r, wsKey, 2)

		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		Expect(ws.Status.Phase).To(Equal(cremav1alpha1.PhaseFailed))
		var survivingPod corev1.Pod
		Expect(k8sClient.Get(ctx, podKey, &survivingPod)).To(Succeed())

		// 4. Recover from commit failure -> commits rev-1 with locked SourceImage="ubuntu:22.04", deletes Pod, enters Suspended.
		agent.commitErr = nil
		runReconcileLoop(ctx, r, wsKey, 2) // Committing -> Committed -> Delete(Pod)
		Expect(agent.lastReq).NotTo(BeNil())
		Expect(agent.lastReq.ParentImageRef).To(Equal("ubuntu:22.04"))

		Expect(k8sClient.Get(ctx, podKey, &survivingPod)).To(Succeed())
		Expect(survivingPod.DeletionTimestamp).NotTo(BeNil())
		Expect(k8sClient.Delete(ctx, &survivingPod, client.GracePeriodSeconds(0))).To(Succeed())

		runReconcileLoop(ctx, r, wsKey, 1) // Pod gone -> Suspended
		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		Expect(ws.Status.Phase).To(Equal(cremav1alpha1.PhaseSuspended))
		Expect(ws.Status.SnapshotSeq).To(Equal(int64(1)))
		Expect(ws.Status.CurrentImage).To(ContainSubstring("registry.crema-system.svc:5000/workspaces/default-" + wsName + "@sha256:"))

		// 5. Resume with updated GPU=2 -> recreates Pod with rev-1 image, gpu=2, and soft affinity to worker-1.
		ws.Spec.Suspend = false
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		runReconcileLoop(ctx, r, wsKey, 1)

		var newPod corev1.Pod
		Expect(k8sClient.Get(ctx, podKey, &newPod)).To(Succeed())
		Expect(newPod.Spec.Containers[0].Image).To(Equal(ws.Status.CurrentImage))
		Expect(newPod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]).To(Equal(resource.MustParse("2")))
		Expect(newPod.Spec.Affinity).NotTo(BeNil())
		Expect(newPod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Preference.MatchExpressions[0].Values).To(ContainElement("worker-1"))

		markPodRunningAndReady(ctx, podKey, "worker-1", "containerd://cid-2", "docker-pullable://ubuntu@sha256:base")
		runReconcileLoop(ctx, r, wsKey, 1)
		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		Expect(ws.Status.Phase).To(Equal(cremav1alpha1.PhaseRunning))

		// 6. Reject base image mutation.
		ws.Spec.Template.Spec.Containers[0].Image = "debian:12"
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		runReconcileLoop(ctx, r, wsKey, 1)
		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		cond = meta.FindStatusCondition(ws.Status.Conditions, condTypeTemplateValid)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("BaseImageImmutable"))
	})
})
