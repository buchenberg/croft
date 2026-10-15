//go:build linux && !nolive

// Live-daemon acceptance for Config.Limits (roadmap decision D2): the spec
// resources must reach the container's cgroup, which only a real runtime can
// prove. The spec-level mapping is pinned by TestInitSpecOpts_Limits; this
// closes the loop through containerd, runc, and cgroup v2.
//
// Run (root, like the rest of the live suite):
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run TestLive_Limits ./...
package croft

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestLive_LimitsReachTheCgroup starts a sandbox under explicit limits and
// reads the cgroup v2 interface files from inside the container: memory.max,
// pids.max, and cpu.max must show exactly the configured values. cpu.weight
// is runc's conversion of the v1-style shares (512 → its own scale), so it
// is logged and sanity-checked rather than pinned to a formula croft does
// not own.
//
// The in-container view depends on the host: containerd gives a cgroup-v2
// daemon's containers a private cgroup namespace and the default spec mounts
// the cgroup filesystem, so these paths show the TASK's own cgroup — but a
// cgroup-v1 host has no v2 interface files to read. The probe below skips
// (honestly, naming both possible causes) rather than false-fails on such a
// host; the spec-level mapping stays pinned by TestInitSpecOpts_Limits
// either way. The probe checks the v2 core interface file, which every v2
// cgroup has: its absence means there is no v2 view in the container at
// all (a v1 host, or no cgroup mount). It cannot tell the task's own
// cgroup from a root-level view, but no such view can pass silently,
// whichever shape the root takes: where the limit files are absent (the
// hierarchy root — they are CFTYPE_NOT_ON_ROOT), cat exits non-zero; where
// a view does expose them reading "max", the value assertions below fail
// on the mismatch.
func TestLive_LimitsReachTheCgroup(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	c := dial(t, cfg.Addr)
	ensureNamespace(t, ctx, c, cfg.Namespace)
	if err := c.Close(); err != nil {
		t.Logf("close setup client: %v", err)
	}

	sb := New(Config{
		Address:     cfg.Addr,
		Namespace:   cfg.Namespace,
		Image:       cfg.Image,
		Snapshotter: cfg.Snapshotter,
		Workdir:     cfg.Workdir,
		FIFODir:     t.TempDir(),
		Limits: &Limits{
			CPUShares:   512,
			CPUQuota:    50000,
			MemoryBytes: 192 << 20,
			Pids:        64,
		},
	})
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Probe the in-container cgroup view before asserting against it.
	probe, err := sb.Exec(ctx, ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "test -r /sys/fs/cgroup/cgroup.controllers"},
		Cwd:     "/",
	})
	if err != nil {
		t.Fatalf("cgroup probe exec: %v", err)
	}
	if probe.ExitCode != 0 {
		t.Skip("no cgroup-v2 view inside the container: the host runs a v1 " +
			"hierarchy, or the daemon provides no cgroup mount/private cgroup " +
			"namespace. The spec-level mapping is pinned by TestInitSpecOpts_Limits; " +
			"this acceptance step needs a v2 daemon host.")
	}

	if got := strings.TrimSpace(mustExec(t, ctx, sb, "/", "cat", "/sys/fs/cgroup/memory.max")); got != "201326592" {
		t.Errorf("memory.max = %q, want 201326592 (192 MiB)", got)
	}
	if got := strings.TrimSpace(mustExec(t, ctx, sb, "/", "cat", "/sys/fs/cgroup/pids.max")); got != "64" {
		t.Errorf("pids.max = %q, want 64", got)
	}
	if got := strings.TrimSpace(mustExec(t, ctx, sb, "/", "cat", "/sys/fs/cgroup/cpu.max")); got != "50000 100000" {
		t.Errorf("cpu.max = %q, want \"50000 100000\" (quota over the default period)", got)
	}
	weight := strings.TrimSpace(mustExec(t, ctx, sb, "/", "cat", "/sys/fs/cgroup/cpu.weight"))
	t.Logf("cpu.weight = %s (runc's conversion of CPUShares 512)", weight)
	if weight == "" || weight == "0" {
		t.Errorf("cpu.weight = %q, want runc's positive shares conversion", weight)
	}
}
