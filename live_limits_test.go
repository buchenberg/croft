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
