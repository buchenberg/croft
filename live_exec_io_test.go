//go:build linux && !nolive

// Live-daemon acceptance for the exec I/O plan: the output cap's truncation
// shape through the real cio plumbing, and the background-process recipe's
// process semantics (reparenting, survival across execs, death at Capture)
// that only a real runtime can prove.
//
// Run (root, like the rest of the live suite):
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run 'TestLive_(ExecOutputCap|BackgroundProcess)' ./...
package croft

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newLiveSandboxWithConfig is newLiveSandbox for the tests that need Config
// knobs beyond the env-driven defaults (MaxOutputBytes here, Limits in the
// D2 suite): same construction, same cleanup registration, plus a mutator.
func newLiveSandboxWithConfig(t *testing.T, ctx context.Context, cfg liveConfig, mutate func(*Config)) *ContainerdSandbox {
	t.Helper()

	c := dial(t, cfg.Addr)
	ensureNamespace(t, ctx, c, cfg.Namespace)
	if err := c.Close(); err != nil {
		t.Logf("close setup client: %v", err)
	}

	sc := Config{
		Address:     cfg.Addr,
		Namespace:   cfg.Namespace,
		Image:       cfg.Image,
		Snapshotter: cfg.Snapshotter,
		Workdir:     cfg.Workdir,
		FIFODir:     t.TempDir(),
	}
	mutate(&sc)

	sb := New(sc)
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})
	return sb
}

// TestLive_ExecOutputCapTruncatesMiddle pins the truncation shape end to end:
// over-cap stdout keeps the first and last half of the budget with the
// marker between, the unstreamed middle is gone, stderr is capped
// independently, and under-cap output is untouched. The expected rendering
// is reconstructed in full — not sampled — so an off-by-one in the budget
// split fails here.
func TestLive_ExecOutputCapTruncatesMiddle(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	const cap4KiB = 4096
	sb := newLiveSandboxWithConfig(t, ctx, cfg, func(c *Config) { c.MaxOutputBytes = cap4KiB })
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The full expected output, reconstructed byte-for-byte.
	var full strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&full, "%d\n", i)
	}
	want := full.String()
	if len(want) <= cap4KiB {
		t.Fatalf("test bug: generated output %d bytes does not exceed the cap", len(want))
	}
	marker := fmt.Sprintf(truncationMarker, len(want)-cap4KiB)
	wantTruncated := want[:cap4KiB/2] + marker + want[len(want)-cap4KiB/2:]

	res, err := sb.Exec(ctx, ExecRequest{
		Command: "seq",
		Args:    []string{"1", "20000"},
		Cwd:     cfg.Workdir,
	})
	if err != nil {
		t.Fatalf("exec seq: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("seq exit = %d (stderr %q)", res.ExitCode, res.Stderr)
	}
	if res.Stdout != wantTruncated {
		t.Errorf("truncated stdout mismatch:\n got head %q...\n got tail ...%q\n len %d, want %d",
			res.Stdout[:min(64, len(res.Stdout))], res.Stdout[max(0, len(res.Stdout)-64):],
			len(res.Stdout), len(wantTruncated))
	}
	if !strings.Contains(res.Stdout, "bytes truncated ...]") {
		t.Error("stdout is missing the truncation marker")
	}

	// stderr is capped on its own budget: same volume, redirected.
	res, err = sb.Exec(ctx, ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "seq 1 20000 1>&2"},
		Cwd:     cfg.Workdir,
	})
	if err != nil {
		t.Fatalf("exec seq >&2: %v", err)
	}
	if res.Stderr != wantTruncated {
		t.Errorf("truncated stderr mismatch: len %d, want %d", len(res.Stderr), len(wantTruncated))
	}
	if res.Stdout != "" {
		t.Errorf("stdout = %q, want empty (the cap applies per stream, not pooled)", res.Stdout)
	}

	// Under-cap output is exact.
	res, err = sb.Exec(ctx, ExecRequest{Command: "echo", Args: []string{"hi"}, Cwd: cfg.Workdir})
	if err != nil {
		t.Fatalf("exec echo: %v", err)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("under-cap stdout = %q, want %q", res.Stdout, "hi\n")
	}
}
