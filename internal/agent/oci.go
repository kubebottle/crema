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
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"
)

const (
	CompressionZstd = "zstd"
	CompressionGzip = "gzip"
)

// DeltaLayer implements v1.Layer backed by a compressed temporary file on disk.
type DeltaLayer struct {
	path        string
	digest      v1.Hash
	diffID      v1.Hash
	size        int64
	mediaType   types.MediaType
	compression string
}

func (l *DeltaLayer) Digest() (v1.Hash, error) { return l.digest, nil }
func (l *DeltaLayer) DiffID() (v1.Hash, error) { return l.diffID, nil }
func (l *DeltaLayer) Size() (int64, error)     { return l.size, nil }
func (l *DeltaLayer) MediaType() (types.MediaType, error) {
	return l.mediaType, nil
}

func (l *DeltaLayer) Compressed() (io.ReadCloser, error) {
	return os.Open(l.path)
}

func (l *DeltaLayer) Uncompressed() (io.ReadCloser, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	switch l.compression {
	case CompressionGzip:
		gr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &compositeReadCloser{Reader: gr, closers: []io.Closer{gr, f}}, nil
	default:
		zr, err := zstd.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &compositeReadCloser{Reader: zr, closers: []io.Closer{zr.IOReadCloser(), f}}, nil
	}
}

// Cleanup removes the backing temporary file.
func (l *DeltaLayer) Cleanup() error {
	if l == nil || l.path == "" {
		return nil
	}
	return os.Remove(l.path)
}

type compositeReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type countingWriter struct {
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// BuildDeltaLayer streams an uncompressed tar from r, extracts DiffStats,
// compresses it using compression ("zstd" or "gzip") into a temp file, and
// returns a v1.Layer ready for OCI image assembly.
func BuildDeltaLayer(r io.Reader, compression string) (*DeltaLayer, DiffStats, error) {
	if compression == "" {
		compression = CompressionZstd
	}
	if compression != CompressionZstd && compression != CompressionGzip {
		return nil, DiffStats{}, fmt.Errorf("unsupported compression %q", compression)
	}

	tmpFile, err := os.CreateTemp("", "crema-delta-*.layer")
	if err != nil {
		return nil, DiffStats{}, fmt.Errorf("create temp layer file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanupOnErr := true
	defer func() {
		_ = tmpFile.Close()
		if cleanupOnErr {
			_ = os.Remove(tmpPath)
		}
	}()

	uncompressedHash := sha256.New()
	compressedHash := sha256.New()
	compressedCounter := &countingWriter{}
	compressedSink := io.MultiWriter(tmpFile, compressedHash, compressedCounter)

	var compWriter io.WriteCloser
	var mediaType types.MediaType
	switch compression {
	case CompressionGzip:
		compWriter = gzip.NewWriter(compressedSink)
		mediaType = types.OCILayer
	default:
		zw, err := zstd.NewWriter(compressedSink, zstd.WithEncoderConcurrency(4))
		if err != nil {
			return nil, DiffStats{}, fmt.Errorf("create zstd writer: %w", err)
		}
		compWriter = zw
		mediaType = types.OCILayerZStd
	}

	pr, pw := io.Pipe()
	type summaryRes struct {
		stats DiffStats
		err   error
	}
	summaryCh := make(chan summaryRes, 1)
	go func() {
		stats, err := SummarizeTar(pr)
		summaryCh <- summaryRes{stats: stats, err: err}
	}()

	mw := io.MultiWriter(uncompressedHash, compWriter, pw)
	_, copyErr := io.Copy(mw, r)
	closeCompErr := compWriter.Close()
	_ = pw.Close()

	sumRes := <-summaryCh
	if copyErr != nil {
		return nil, DiffStats{}, fmt.Errorf("stream delta tar: %w", copyErr)
	}
	if closeCompErr != nil {
		return nil, DiffStats{}, fmt.Errorf("finalize compressed layer: %w", closeCompErr)
	}
	if sumRes.err != nil {
		return nil, DiffStats{}, fmt.Errorf("summarize delta tar: %w", sumRes.err)
	}

	layer := &DeltaLayer{
		path: tmpPath,
		digest: v1.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(compressedHash.Sum(nil)),
		},
		diffID: v1.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(uncompressedHash.Sum(nil)),
		},
		size:        compressedCounter.n,
		mediaType:   mediaType,
		compression: compression,
	}
	cleanupOnErr = false
	return layer, sumRes.stats, nil
}

// PushOptions configures parent image resolution and target OCI push.
type PushOptions struct {
	ParentImageRef        string
	TargetRepository      string
	TargetTag             string
	PlainHTTP             bool
	InsecureSkipTLSVerify bool
	DockerConfigJSON      []byte
}

// PushResult contains the immutable reference and layer metadata after push.
type PushResult struct {
	ImageRef       string
	ImageDigest    string
	LayerDigest    string
	LayerSizeBytes int64
}

// PushIncrementalImage fetches the parent OCI image, appends layer, converts to
// OCI manifest/config media types, and pushes to the target repository.
func PushIncrementalImage(ctx context.Context, opts PushOptions, layer *DeltaLayer) (PushResult, error) {
	if opts.TargetTag == "" {
		opts.TargetTag = "latest"
	}

	targetNameOpts := []name.Option{}
	if opts.PlainHTTP {
		targetNameOpts = append(targetNameOpts, name.Insecure)
	}
	targetRef, err := name.NewTag(opts.TargetRepository+":"+opts.TargetTag, targetNameOpts...)
	if err != nil {
		return PushResult{}, fmt.Errorf("parse target ref: %w", err)
	}
	targetRegistry := targetRef.Context().RegistryStr()

	parentNameOpts := []name.Option{}
	if opts.PlainHTTP && strings.HasPrefix(opts.ParentImageRef, targetRegistry+"/") {
		parentNameOpts = append(parentNameOpts, name.Insecure)
	}
	parentRef, err := name.ParseReference(opts.ParentImageRef, parentNameOpts...)
	if err != nil {
		return PushResult{}, fmt.Errorf("parse parent ref %q: %w", opts.ParentImageRef, err)
	}

	keychain, err := buildKeychain(opts.DockerConfigJSON)
	if err != nil {
		return PushResult{}, fmt.Errorf("parse registry credentials: %w", err)
	}

	tr := buildTransport(targetRegistry, opts.PlainHTTP, opts.InsecureSkipTLSVerify)
	remoteOpts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(keychain),
		remote.WithTransport(tr),
	}

	parentImg, err := remote.Image(parentRef, remoteOpts...)
	if err != nil {
		return PushResult{}, fmt.Errorf("fetch parent image %q: %w", parentRef.Name(), err)
	}

	newImg, err := mutate.AppendLayers(parentImg, layer)
	if err != nil {
		return PushResult{}, fmt.Errorf("append delta layer: %w", err)
	}
	newImg = mutate.MediaType(newImg, types.OCIManifestSchema1)
	newImg = mutate.ConfigMediaType(newImg, types.OCIConfigJSON)

	if err := remote.Write(targetRef, newImg, remoteOpts...); err != nil {
		return PushResult{}, fmt.Errorf("push image %q: %w", targetRef.Name(), err)
	}

	imgDigest, err := newImg.Digest()
	if err != nil {
		return PushResult{}, fmt.Errorf("compute manifest digest: %w", err)
	}
	layerDigest, err := layer.Digest()
	if err != nil {
		return PushResult{}, fmt.Errorf("get layer digest: %w", err)
	}
	layerSize, err := layer.Size()
	if err != nil {
		return PushResult{}, fmt.Errorf("get layer size: %w", err)
	}

	return PushResult{
		ImageRef:       fmt.Sprintf("%s@%s", targetRef.Context().Name(), imgDigest.String()),
		ImageDigest:    imgDigest.String(),
		LayerDigest:    layerDigest.String(),
		LayerSizeBytes: layerSize,
	}, nil
}

type hostAwareTransport struct {
	base           http.RoundTripper
	plainHTTPHosts map[string]bool
}

func (t *hostAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.plainHTTPHosts[req.URL.Host] && req.URL.Scheme == "https" {
		cloned := req.Clone(req.Context())
		cloned.URL.Scheme = "http"
		return t.base.RoundTrip(cloned)
	}
	return t.base.RoundTrip(req)
}

func buildTransport(targetRegistry string, plainHTTP, insecureSkipTLS bool) http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkipTLS {
		base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // user-configured registry option
	}
	if !plainHTTP {
		return base
	}
	return &hostAwareTransport{
		base:           base,
		plainHTTPHosts: map[string]bool{targetRegistry: true},
	}
}

type dockerConfig struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

type staticKeychain struct {
	auths map[string]authn.AuthConfig
}

func (k *staticKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	reg := res.RegistryStr()
	if cfg, ok := k.auths[reg]; ok {
		return authn.FromConfig(cfg), nil
	}
	for host, cfg := range k.auths {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
		trimmed = strings.TrimSuffix(trimmed, "/")
		if trimmed == reg {
			return authn.FromConfig(cfg), nil
		}
	}
	return authn.Anonymous, nil
}

func buildKeychain(raw []byte) (authn.Keychain, error) {
	if len(raw) == 0 {
		return authn.DefaultKeychain, nil
	}
	var cfg dockerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	auths := make(map[string]authn.AuthConfig, len(cfg.Auths))
	for k, v := range cfg.Auths {
		ac := authn.AuthConfig{
			Username: v.Username,
			Password: v.Password,
		}
		if v.Auth != "" && ac.Username == "" && ac.Password == "" {
			decoded, err := base64.StdEncoding.DecodeString(v.Auth)
			if err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 {
					ac.Username = parts[0]
					ac.Password = parts[1]
				}
			}
		}
		auths[k] = ac
	}
	return &staticKeychain{auths: auths}, nil
}
