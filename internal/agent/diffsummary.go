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
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	maxSummarySamples = 5
	maxSummaryLen     = 200
)

// DiffStats holds counts and sample paths extracted from a delta tar stream.
type DiffStats struct {
	Changed int
	Deleted int
	Opaque  int
	Samples []string
}

// Empty returns true if no filesystem entries were recorded in the delta tar.
func (s DiffStats) Empty() bool {
	return s.Changed == 0 && s.Deleted == 0 && s.Opaque == 0
}

// String formats DiffStats as a compact human-readable summary.
func (s DiffStats) String() string {
	if s.Empty() {
		return "no changes"
	}
	var parts []string
	if s.Changed > 0 {
		parts = append(parts, fmt.Sprintf("~%d", s.Changed))
	}
	if s.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("-%d", s.Deleted))
	}
	if s.Opaque > 0 {
		parts = append(parts, fmt.Sprintf("opq:%d", s.Opaque))
	}
	summary := strings.Join(parts, " ")
	if len(s.Samples) > 0 {
		joined := strings.Join(s.Samples, ", ")
		candidate := fmt.Sprintf("%s (%s)", summary, joined)
		if len(candidate) > maxSummaryLen {
			candidate = candidate[:maxSummaryLen-3] + "..."
		}
		return candidate
	}
	return summary
}

// SummarizeTar scans tar headers from r and returns compact diff statistics.
// It consumes r until io.EOF so callers using io.Pipe / io.TeeReader do not block.
func SummarizeTar(r io.Reader) (DiffStats, error) {
	tr := tar.NewReader(r)
	var stats DiffStats

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("read tar header: %w", err)
		}

		clean := strings.TrimPrefix(path.Clean(hdr.Name), "./")
		if clean == "." || clean == "" {
			continue
		}

		base := path.Base(clean)
		dir := path.Dir(clean)

		switch {
		case base == ".wh..wh..opq":
			stats.Opaque++
		case strings.HasPrefix(base, ".wh."):
			stats.Deleted++
			orig := strings.TrimPrefix(base, ".wh.")
			if dir != "." {
				orig = path.Join(dir, orig)
			}
			if len(stats.Samples) < maxSummarySamples {
				stats.Samples = append(stats.Samples, "-"+orig)
			}
		case hdr.Typeflag != tar.TypeDir:
			stats.Changed++
			if len(stats.Samples) < maxSummarySamples {
				stats.Samples = append(stats.Samples, clean)
			}
		}
	}

	// Drain any trailing zero blocks.
	_, _ = io.Copy(io.Discard, r)
	return stats, nil
}
