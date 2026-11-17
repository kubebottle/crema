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
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestBuildDeltaLayerAndPushIncrementalImage(t *testing.T) {
	// Start an in-memory OCI registry over plain HTTP.
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	regHost := u.Host

	// Push a synthetic base image to the in-memory registry.
	baseTar := buildTestTar(t, []struct {
		name     string
		body     string
		typeflag byte
	}{
		{name: "etc/hello.txt", body: "hello base\n"},
	})
	baseLayer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(baseTar)), nil
	})
	if err != nil {
		t.Fatalf("create base layer: %v", err)
	}
	baseImg, err := mutate.AppendLayers(empty.Image, baseLayer)
	if err != nil {
		t.Fatalf("append base layer: %v", err)
	}
	baseRefStr := regHost + "/base/ubuntu:22.04"
	baseRef, err := name.ParseReference(baseRefStr, name.Insecure)
	if err != nil {
		t.Fatalf("parse base ref: %v", err)
	}
	if err := remote.Write(baseRef, baseImg); err != nil {
		t.Fatalf("write base image: %v", err)
	}

	// Build a zstd delta layer from a tar stream.
	deltaTar := buildTestTar(t, []struct {
		name     string
		body     string
		typeflag byte
	}{
		{name: "root/.bashrc", body: "alias ll='ls -la'\n"},
		{name: "etc/.wh.hello.txt"},
	})
	deltaLayer, stats, err := BuildDeltaLayer(bytes.NewReader(deltaTar), CompressionZstd)
	if err != nil {
		t.Fatalf("BuildDeltaLayer error: %v", err)
	}
	defer func() { _ = deltaLayer.Cleanup() }()

	if stats.Changed != 1 || stats.Deleted != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	mt, err := deltaLayer.MediaType()
	if err != nil || mt != types.OCILayerZStd {
		t.Fatalf("expected OCILayerZStd, got %v (err=%v)", mt, err)
	}

	// Push incremental image.
	targetRepo := regHost + "/workspaces/default-ws"
	res, err := PushIncrementalImage(context.Background(), PushOptions{
		ParentImageRef:   baseRefStr,
		TargetRepository: targetRepo,
		TargetTag:        "rev-1",
		PlainHTTP:        true,
	}, deltaLayer)
	if err != nil {
		t.Fatalf("PushIncrementalImage error: %v", err)
	}
	if res.ImageDigest == "" || res.LayerDigest == "" || res.LayerSizeBytes <= 0 {
		t.Fatalf("invalid push result: %+v", res)
	}

	// Verify pushed image has 2 layers (base + delta) and OCIManifestSchema1.
	pushedRef, err := name.ParseReference(res.ImageRef, name.Insecure)
	if err != nil {
		t.Fatalf("parse pushed ref: %v", err)
	}
	pushedImg, err := remote.Image(pushedRef)
	if err != nil {
		t.Fatalf("fetch pushed image: %v", err)
	}
	layers, err := pushedImg.Layers()
	if err != nil {
		t.Fatalf("get pushed layers: %v", err)
	}
	if len(layers) != 2 {
		t.Fatalf("expected 2 layers, got %d", len(layers))
	}
	cfg, err := pushedImg.ConfigFile()
	if err != nil {
		t.Fatalf("get pushed config: %v", err)
	}
	if len(cfg.RootFS.DiffIDs) != 2 {
		t.Fatalf("expected 2 diff_ids, got %d", len(cfg.RootFS.DiffIDs))
	}
}
