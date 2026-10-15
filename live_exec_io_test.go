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

// startBackgroundTicker launches a writer that appends a tick line to
// /tmp/bg.log every 200ms, via the documented recipe: nohup, redirected,
// backgrounded. The launching Exec must return immediately — the point of the
// recipe is that Exec waits for its command, and this command's job is to
// not be the workload.
func startBackgroundTicker(t *testing.T, ctx context.Context, sb *ContainerdSandbox, cwd string) {
	t.Helper()
	start := time.Now()
	res, err := sb.Exec(ctx, ExecRequest{
		Command: "/bin/sh",
		Args: []string{"-c",
			`nohup /bin/sh -c 'i=0; while [ $i -lt 1000 ]; do echo "tick $i" >> /tmp/bg.log; i=$((i+1)); sleep 0.2; done' >/dev/null 2>&1 &`},
		Cwd: cwd,
	})
	if err != nil {
		t.Fatalf("launch background ticker: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("launch exit = %d (stderr %q)", res.ExitCode, res.Stderr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("launch took %s, want an immediate return (the workload is backgrounded)", elapsed)
	}
}

// tickCount reads /tmp/bg.log and returns its line count, failing the test
// only when the read itself fails (an empty log before the first tick is a
// legitimate zero).
func tickCount(t *testing.T, ctx context.Context, sb *ContainerdSandbox) int {
	t.Helper()
	data, err := sb.ReadFile(ctx, "/tmp/bg.log")
	if err != nil {
		t.Fatalf("read bg log: %v", err)
	}
	return strings.Count(string(data), "\n")
}

// waitTicks polls until the log holds at least want lines, or fails the test
// after the deadline.
func waitTicks(t *testing.T, ctx context.Context, sb *ContainerdSandbox, want int) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		n := tickCount(t, ctx, sb)
		if n >= want {
			return n
		}
		if time.Now().After(deadline) {
			t.Fatalf("bg log held %d lines after 15s, want >= %d (the background process never ran)", n, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestLive_BackgroundProcessSurvivesExec pins the recipe's product behavior
// on a real runtime: a backgrounded writer survives the exec that launched
// it (reparented to the container's init) and keeps producing across later
// execs and file reads — the log grows while the client merely polls.
func TestLive_BackgroundProcessSurvivesExec(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	startBackgroundTicker(t, ctx, sb, cfg.Workdir)

	// The process is alive across unrelated client traffic: first ticks
	// appear, and the count keeps growing afterwards.
	first := waitTicks(t, ctx, sb, 2)
	time.Sleep(700 * time.Millisecond)
	second := tickCount(t, ctx, sb)
	if second <= first {
		t.Errorf("bg log stuck at %d lines (was %d): the process did not survive its launching exec", second, first)
	}
}

// TestLive_BackgroundProcessDiesAtCapture pins the recipe's boundary with
// the same honesty: Capture stops the task to commit a coherent layer, so
// the background process is gone afterwards — while its FILE output persists
// in the snapshot. A growing-then-frozen log is the observation; a
// process-preserving capture is a permanent scope boundary, not a gap.
func TestLive_BackgroundProcessDiesAtCapture(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	startBackgroundTicker(t, ctx, sb, cfg.Workdir)
	before := waitTicks(t, ctx, sb, 2)

	state := captureLive(t, ctx, sb)
	if state.SnapshotKey == "" {
		t.Fatal("Capture returned an empty state")
	}

	// After the restart the writer must be dead: the log it kept growing
	// freezes at whatever was committed (possibly a few ticks more than
	// `before` — the process ran until the stop), and stays frozen while a
	// live writer would add several lines per second.
	time.Sleep(1500 * time.Millisecond)
	frozen := tickCount(t, ctx, sb)
	if frozen < before {
		t.Errorf("bg log shrank across Capture: %d → %d (the committed layer must carry the file)", before, frozen)
	}
	time.Sleep(1500 * time.Millisecond)
	if grown := tickCount(t, ctx, sb); grown != frozen {
		t.Errorf("bg log grew %d → %d after Capture: the background process survived a snapshot it must not survive", frozen, grown)
	}
}
