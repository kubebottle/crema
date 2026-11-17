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

package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/kubebottle/crema/internal/agent"
	"github.com/kubebottle/crema/internal/agentpb"
)

func main() {
	var (
		listenAddr       string
		containerdSocket string
		nodeName         string
	)
	flag.StringVar(&listenAddr, "listen-addr", ":9090", "gRPC listen address")
	flag.StringVar(&containerdSocket, "containerd-socket", "/run/containerd/containerd.sock", "Path to containerd socket")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Kubernetes node name")
	flag.Parse()

	if nodeName == "" {
		if h, err := os.Hostname(); err == nil {
			nodeName = h
		}
	}

	committer, err := agent.NewContainerdCommitter(containerdSocket)
	if err != nil {
		log.Fatalf("failed to connect containerd: %v", err)
	}
	defer func() { _ = committer.Close() }()

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("failed to listen %s: %v", listenAddr, err)
	}

	grpcServer := grpc.NewServer()
	agentpb.RegisterCremaAgentServer(grpcServer, agent.NewServer(committer, nodeName))

	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, hs)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received signal %v, shutting down gRPC server", sig)
		grpcServer.GracefulStop()
	}()

	log.Printf("crema-agent listening on %s (node=%s, containerd=%s)", listenAddr, nodeName, containerdSocket)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("gRPC serve failed: %v", err)
	}
}
