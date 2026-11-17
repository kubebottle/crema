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
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

func buildTestTar(t *testing.T, entries []struct {
	name     string
	body     string
	typeflag byte
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0644,
			Size:     int64(len(e.body)),
			Typeflag: tf,
		}
		if tf == tar.TypeDir {
			hdr.Mode = 0755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %s: %v", e.name, err)
		}
		if len(e.body) > 0 && tf != tar.TypeDir {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write tar body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func TestSummarizeTar(t *testing.T) {
	raw := buildTestTar(t, []struct {
		name     string
		body     string
		typeflag byte
	}{
		{name: "./root/", typeflag: tar.TypeDir},
		{name: "./root/.bashrc", body: "export FOO=1\n"},
		{name: "usr/bin/jq", body: "binary"},
		{name: "etc/.wh.old-conf"},
		{name: "var/cache/.wh..wh..opq"},
	})

	stats, err := SummarizeTar(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("SummarizeTar error: %v", err)
	}
	if stats.Changed != 2 {
		t.Fatalf("expected Changed=2, got %d", stats.Changed)
	}
	if stats.Deleted != 1 {
		t.Fatalf("expected Deleted=1, got %d", stats.Deleted)
	}
	if stats.Opaque != 1 {
		t.Fatalf("expected Opaque=1, got %d", stats.Opaque)
	}
	s := stats.String()
	if !strings.Contains(s, "~2") || !strings.Contains(s, "-1") || !strings.Contains(s, "-etc/old-conf") {
		t.Fatalf("unexpected summary string: %q", s)
	}
}

func TestSummarizeTarEmpty(t *testing.T) {
	raw := buildTestTar(t, nil)
	stats, err := SummarizeTar(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("SummarizeTar error: %v", err)
	}
	if !stats.Empty() || stats.String() != "no changes" {
		t.Fatalf("expected empty summary, got %+v (%q)", stats, stats.String())
	}
}
