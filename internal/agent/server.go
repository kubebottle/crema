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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kubebottle/crema/internal/agentpb"
)

// Server implements agentpb.CremaAgentServer backed by ContainerdCommitter.
type Server struct {
	agentpb.UnimplementedCremaAgentServer
	committer *ContainerdCommitter
	nodeName  string
}

// NewServer constructs a gRPC CremaAgentServer.
func NewServer(committer *ContainerdCommitter, nodeName string) *Server {
	return &Server{
		committer: committer,
		nodeName:  nodeName,
	}
}

func (s *Server) CommitContainer(ctx context.Context, req *agentpb.CommitContainerRequest) (*agentpb.CommitContainerResponse, error) {
	if req.GetOperationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	if req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container_id is required")
	}
	if req.GetTargetRepository() == "" {
		return nil, status.Error(codes.InvalidArgument, "target_repository is required")
	}

	out, err := s.committer.CommitContainer(ctx, CommitParams{
		OperationID:      req.GetOperationId(),
		Namespace:        req.GetWorkspaceNamespace(),
		PodName:          req.GetPodName(),
		PodUID:           req.GetPodUid(),
		ContainerName:    req.GetContainerName(),
		ContainerID:      req.GetContainerId(),
		ParentImageRef:   req.GetParentImageRef(),
		TargetRepository: req.GetTargetRepository(),
		TargetTag:        req.GetTargetTag(),
		Compression:      req.GetCompression(),
		PlainHTTP:        req.GetPlainHttp(),
		InsecureSkipTLS:  req.GetInsecureSkipTlsVerify(),
		DockerConfigJSON: req.GetDockerConfigJson(),
		KeepQuiesced:     req.GetKeepQuiesced(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "commit container: %v", err)
	}

	return &agentpb.CommitContainerResponse{
		ImageRef:       out.ImageRef,
		ImageDigest:    out.ImageDigest,
		LayerDigest:    out.LayerDigest,
		LayerSizeBytes: out.LayerSizeBytes,
		DiffSummary:    out.DiffSummary,
	}, nil
}

func (s *Server) Health(ctx context.Context, _ *agentpb.HealthRequest) (*agentpb.HealthResponse, error) {
	ver, err := s.committer.Version(ctx)
	if err != nil {
		return &agentpb.HealthResponse{
			Healthy:  false,
			NodeName: s.nodeName,
		}, nil
	}
	return &agentpb.HealthResponse{
		Healthy:           true,
		ContainerdVersion: ver,
		NodeName:          s.nodeName,
	}, nil
}
