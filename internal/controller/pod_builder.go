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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cremav1alpha1 "github.com/kubebottle/crema/api/v1alpha1"
)

const (
	labelWorkspaceName = "crema.kubebottle.io/workspace"
	labelTemplateHash  = "crema.kubebottle.io/template-hash"
	nodeHostnameKey    = "kubernetes.io/hostname"
)

// WorkspacePodName returns the deterministic Pod name for a GPUWorkspace.
func WorkspacePodName(wsName string) string {
	return fmt.Sprintf("%s-ws", wsName)
}

// WorkspacePVCName returns the StatefulSet-style PVC name for a volumeClaimTemplate.
func WorkspacePVCName(wsName, claimName string) string {
	return fmt.Sprintf("%s-%s", wsName, claimName)
}

// ComputeTemplateHash hashes spec.template while ignoring containers[0].image so
// base image immutability is checked separately from template mutations.
func ComputeTemplateHash(tpl corev1.PodTemplateSpec) (string, error) {
	cloned := tpl.DeepCopy()
	if len(cloned.Spec.Containers) > 0 {
		cloned.Spec.Containers[0].Image = ""
	}
	raw, err := json.Marshal(cloned)
	if err != nil {
		return "", fmt.Errorf("marshal template for hash: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

// BuildWorkspacePod constructs the workspace Pod from spec.template using image
// for containers[0], binding VolumeClaimTemplates, and injecting soft node
// affinity toward ws.Status.NodeName when available.
func BuildWorkspacePod(ws *cremav1alpha1.GPUWorkspace, image, templateHash string) *corev1.Pod {
	labels := make(map[string]string, len(ws.Spec.Template.Labels)+2)
	for k, v := range ws.Spec.Template.Labels {
		labels[k] = v
	}
	labels[labelWorkspaceName] = ws.Name
	labels[labelTemplateHash] = templateHash

	annotations := make(map[string]string, len(ws.Spec.Template.Annotations))
	for k, v := range ws.Spec.Template.Annotations {
		annotations[k] = v
	}

	podSpec := ws.Spec.Template.Spec.DeepCopy()
	if len(podSpec.Containers) > 0 {
		podSpec.Containers[0].Image = image
	}

	// Ensure VolumeClaimTemplates are wired into podSpec.Volumes if not explicitly declared.
	existingVols := make(map[string]bool, len(podSpec.Volumes))
	for _, v := range podSpec.Volumes {
		existingVols[v.Name] = true
	}
	for _, vct := range ws.Spec.VolumeClaimTemplates {
		if existingVols[vct.Metadata.Name] {
			continue
		}
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: vct.Metadata.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: WorkspacePVCName(ws.Name, vct.Metadata.Name),
				},
			},
		})
	}

	// Inject soft node affinity toward the last known node to reuse cached base layers.
	if ws.Status.NodeName != "" {
		injectSoftNodeAffinity(podSpec, ws.Status.NodeName)
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        WorkspacePodName(ws.Name),
			Namespace:   ws.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *podSpec,
	}
}

func injectSoftNodeAffinity(podSpec *corev1.PodSpec, nodeName string) {
	term := corev1.PreferredSchedulingTerm{
		Weight: 100,
		Preference: corev1.NodeSelectorTerm{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      nodeHostnameKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{nodeName},
				},
			},
		},
	}
	if podSpec.Affinity == nil {
		podSpec.Affinity = &corev1.Affinity{}
	}
	if podSpec.Affinity.NodeAffinity == nil {
		podSpec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	podSpec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(
		podSpec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
		term,
	)
}
