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
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubebottle/crema/internal/agentpb"
)

const (
	defaultAgentPort     = 9090
	defaultCommitTimeout = 10 * time.Minute
	defaultConfirmTimout = 15 * time.Second
)

// AgentClient abstracts node-local crema-agent RPCs for testability.
type AgentClient interface {
	CommitContainer(ctx context.Context, nodeName string, req *agentpb.CommitContainerRequest) (*agentpb.CommitContainerResponse, error)
	ConfirmQuiesced(ctx context.Context, nodeName string, req *agentpb.ConfirmQuiescedRequest) (*agentpb.ConfirmQuiescedResponse, error)
}

// GRPCAgentClient discovers the Ready crema-agent Pod on a target node and calls its gRPC endpoint.
type GRPCAgentClient struct {
	Client         client.Client
	AgentNamespace string
	AgentSelector  map[string]string
	Port           int
}

func (c *GRPCAgentClient) resolveAgentAddress(ctx context.Context, nodeName string) (string, error) {
	if nodeName == "" {
		return "", fmt.Errorf("nodeName is empty")
	}

	selector := c.AgentSelector
	if len(selector) == 0 {
		selector = map[string]string{"app.kubernetes.io/name": "crema-agent"}
	}

	var listOpts []client.ListOption
	listOpts = append(listOpts, client.MatchingLabels(selector))
	if c.AgentNamespace != "" {
		listOpts = append(listOpts, client.InNamespace(c.AgentNamespace))
	}

	var pods corev1.PodList
	if err := c.Client.List(ctx, &pods, listOpts...); err != nil {
		return "", fmt.Errorf("list crema-agent pods: %w", err)
	}

	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != nodeName || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		if isPodReady(p) {
			port := c.Port
			if port <= 0 {
				port = defaultAgentPort
			}
			return fmt.Sprintf("%s:%d", p.Status.PodIP, port), nil
		}
	}

	return "", fmt.Errorf("no Ready crema-agent pod found on node %q", nodeName)
}

func (c *GRPCAgentClient) CommitContainer(ctx context.Context, nodeName string, req *agentpb.CommitContainerRequest) (*agentpb.CommitContainerResponse, error) {
	addr, err := c.resolveAgentAddress(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial crema-agent %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	callCtx, cancel := context.WithTimeout(ctx, defaultCommitTimeout)
	defer cancel()

	return agentpb.NewCremaAgentClient(conn).CommitContainer(callCtx, req)
}

func (c *GRPCAgentClient) ConfirmQuiesced(ctx context.Context, nodeName string, req *agentpb.ConfirmQuiescedRequest) (*agentpb.ConfirmQuiescedResponse, error) {
	addr, err := c.resolveAgentAddress(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial crema-agent %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	callCtx, cancel := context.WithTimeout(ctx, defaultConfirmTimout)
	defer cancel()

	return agentpb.NewCremaAgentClient(conn).ConfirmQuiesced(callCtx, req)
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
