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

package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	k8sNamespace       = "k8s.io"
	labelPodUID        = "io.kubernetes.pod.uid"
	labelPodName       = "io.kubernetes.pod.name"
	labelPodNamespace  = "io.kubernetes.pod.namespace"
	labelContainerName = "io.kubernetes.container.name"
)

// ContainerdCommitter interacts with containerd over its UNIX socket to capture
// container rootfs deltas and push incremental OCI images.
type ContainerdCommitter struct {
	client *containerd.Client

	mu      sync.Mutex
	opLocks map[string]*sync.Mutex
}

// NewContainerdCommitter connects to the given containerd socket path.
func NewContainerdCommitter(socketPath string) (*ContainerdCommitter, error) {
	cli, err := containerd.New(socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect containerd %s: %w", socketPath, err)
	}
	return &ContainerdCommitter{
		client:  cli,
		opLocks: make(map[string]*sync.Mutex),
	}, nil
}

// Close closes the underlying containerd client connection.
func (c *ContainerdCommitter) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Version returns the connected containerd daemon version.
func (c *ContainerdCommitter) Version(ctx context.Context) (string, error) {
	ver, err := c.client.Version(ctx)
	if err != nil {
		return "", err
	}
	return ver.Version, nil
}

func (c *ContainerdCommitter) lockOperation(opID string) func() {
	c.mu.Lock()
	l, ok := c.opLocks[opID]
	if !ok {
		l = &sync.Mutex{}
		c.opLocks[opID] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// CommitParams describes a container commit request.
type CommitParams struct {
	OperationID      string
	Namespace        string
	PodName          string
	PodUID           string
	ContainerName    string
	ContainerID      string
	ParentImageRef   string
	TargetRepository string
	TargetTag        string
	Compression      string
	PlainHTTP        bool
	InsecureSkipTLS  bool
	DockerConfigJSON []byte
	KeepQuiesced     bool
}

// CommitOutcome describes the result of committing and pushing a container delta.
type CommitOutcome struct {
	ImageRef       string
	ImageDigest    string
	LayerDigest    string
	LayerSizeBytes int64
	DiffSummary    string
}

// CommitContainer freezes the container task (if running), extracts the rootfs
// delta via containerd DiffService.Compare, and builds & pushes the OCI image.
// On failure, any task paused by this call is resumed immediately so the source Pod
// remains usable; on success with KeepQuiesced=true, the task stays paused until
// the controller deletes the Pod.
func (c *ContainerdCommitter) CommitContainer(ctx context.Context, p CommitParams) (CommitOutcome, error) {
	if p.OperationID == "" {
		return CommitOutcome{}, fmt.Errorf("operation_id is required")
	}
	unlock := c.lockOperation(p.OperationID)
	defer unlock()

	containerID := strings.TrimPrefix(p.ContainerID, "containerd://")
	if containerID == "" {
		return CommitOutcome{}, fmt.Errorf("container_id is required")
	}

	ctx = namespaces.WithNamespace(ctx, k8sNamespace)

	ctr, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("load container %s: %w", containerID, err)
	}
	info, err := ctr.Info(ctx)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("container info %s: %w", containerID, err)
	}
	if err := validateContainerIdentity(info.Labels, p); err != nil {
		return CommitOutcome{}, err
	}

	sn := c.client.SnapshotService(info.Snapshotter)
	stat, err := sn.Stat(ctx, info.SnapshotKey)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("stat snapshot %s: %w", info.SnapshotKey, err)
	}

	parentImageRef := p.ParentImageRef
	if parentImageRef == "" {
		parentImageRef = info.Image
	}

	// Pause running task to ensure filesystem quiescence during diff.Compare.
	var pausedTask containerd.Task
	task, err := ctr.Task(ctx, nil)
	if err == nil {
		st, stErr := task.Status(ctx)
		if stErr == nil && st.Status == containerd.Running {
			if err := task.Pause(ctx); err != nil {
				return CommitOutcome{}, fmt.Errorf("pause task %s: %w", containerID, err)
			}
			pausedTask = task
		}
	} else if !errdefs.IsNotFound(err) {
		return CommitOutcome{}, fmt.Errorf("lookup task %s: %w", containerID, err)
	}

	// Always resume on error or when KeepQuiesced is false.
	keepPaused := false
	defer func() {
		if pausedTask != nil && !keepPaused {
			resumeCtx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), k8sNamespace), 10*time.Second)
			defer cancel()
			_ = pausedTask.Resume(resumeCtx)
		}
	}()

	// Create readonly view of parent snapshot and compute diff against active snapshot.
	viewKey := fmt.Sprintf("crema-view-%s-%d", p.OperationID, time.Now().UnixNano())
	lowerMounts, err := sn.View(ctx, viewKey, stat.Parent)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("create parent snapshot view: %w", err)
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), k8sNamespace), 10*time.Second)
		defer cancel()
		_ = sn.Remove(rmCtx, viewKey)
	}()

	upperMounts, err := sn.Mounts(ctx, info.SnapshotKey)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("get active snapshot mounts: %w", err)
	}

	diffDesc, err := c.client.DiffService().Compare(
		ctx,
		lowerMounts,
		upperMounts,
		diff.WithMediaType(ocispec.MediaTypeImageLayer),
	)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("containerd diff.Compare: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), k8sNamespace), 10*time.Second)
		defer cancel()
		_ = c.client.ContentStore().Delete(delCtx, diffDesc.Digest)
	}()

	if !p.KeepQuiesced && pausedTask != nil {
		if err := pausedTask.Resume(ctx); err == nil {
			pausedTask = nil
		}
	}

	ra, err := c.client.ContentStore().ReaderAt(ctx, diffDesc)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("open diff content reader: %w", err)
	}
	defer func() { _ = ra.Close() }()

	layer, stats, err := BuildDeltaLayer(content.NewReader(ra), p.Compression)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("build delta layer: %w", err)
	}
	defer func() { _ = layer.Cleanup() }()

	pushRes, err := PushIncrementalImage(ctx, PushOptions{
		ParentImageRef:        parentImageRef,
		TargetRepository:      p.TargetRepository,
		TargetTag:             p.TargetTag,
		PlainHTTP:             p.PlainHTTP,
		InsecureSkipTLSVerify: p.InsecureSkipTLS,
		DockerConfigJSON:      p.DockerConfigJSON,
	}, layer)
	if err != nil {
		return CommitOutcome{}, fmt.Errorf("push incremental image: %w", err)
	}

	if p.KeepQuiesced {
		keepPaused = true
	}

	return CommitOutcome{
		ImageRef:       pushRes.ImageRef,
		ImageDigest:    pushRes.ImageDigest,
		LayerDigest:    pushRes.LayerDigest,
		LayerSizeBytes: pushRes.LayerSizeBytes,
		DiffSummary:    stats.String(),
	}, nil
}

func validateContainerIdentity(labels map[string]string, p CommitParams) error {
	if p.PodUID != "" {
		if got := labels[labelPodUID]; got != p.PodUID {
			return fmt.Errorf("pod UID mismatch: expected %q, got %q", p.PodUID, got)
		}
	}
	if p.Namespace != "" {
		if got := labels[labelPodNamespace]; got != p.Namespace {
			return fmt.Errorf("pod namespace mismatch: expected %q, got %q", p.Namespace, got)
		}
	}
	if p.PodName != "" {
		if got := labels[labelPodName]; got != p.PodName {
			return fmt.Errorf("pod name mismatch: expected %q, got %q", p.PodName, got)
		}
	}
	if p.ContainerName != "" {
		if got := labels[labelContainerName]; got != p.ContainerName {
			return fmt.Errorf("container name mismatch: expected %q, got %q", p.ContainerName, got)
		}
	}
	return nil
}
