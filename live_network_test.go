//go:build linux && !nolive

// Live-daemon acceptance for Config.Network (phase 5): the posture must be
// visible in the container's own network view, which only a real runtime can
// prove. The spec-level mapping is pinned by TestInitSpecOpts_Network; this
// closes the loop through containerd and runc.
//
// The proof is `ls /sys/class/net` — image-contract tools only. There is no
// egress probe: curl and wget are not in the image contract and POSIX sh has
// no /dev/tcp to reach for, so the interface listing is the contract-honest
// statement of the posture, and the docs say so plainly rather than staging
// a curl image for the test.
//
// Run (root, like the rest of the live suite):
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run TestLive_Network ./...
package croft

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// startNetSandbox creates a sandbox under the given Network posture and
// registers its teardown, mirroring the other live tests' lifecycle.
func startNetSandbox(t *testing.T, network string) (*ContainerdSandbox, context.Context) {
	t.Helper()
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	t.Cleanup(cancel)

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
		Network:     network,
	})
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create (Network %q): %v", network, err)
	}
	return sb, ctx
}

// netInterfaces lists /sys/class/net inside the sandbox, sorted.
func netInterfaces(t *testing.T, ctx context.Context, sb *ContainerdSandbox) []string {
	t.Helper()
	got := strings.Fields(strings.TrimSpace(
		mustExec(t, ctx, sb, "/", "ls", "/sys/class/net")))
	sort.Strings(got)
	return got
}

// TestLive_NetworkNoneIsLoopbackOnly pins the off-switch — and, with the
// empty default checked alongside, the ruling that made phase 5 honest: ""
// and "none" are the same posture (the private netns containerd's default
// spec has always asked for, in which runc leaves nothing but loopback),
// and that posture is what croft has shipped by default since day one.
func TestLive_NetworkNoneIsLoopbackOnly(t *testing.T) {
	for _, network := range []string{"none", ""} {
		sb, ctx := startNetSandbox(t, network)
		if got := netInterfaces(t, ctx, sb); len(got) != 1 || got[0] != "lo" {
			t.Errorf("Network %q: interfaces = %v, want exactly [lo] — the "+
				"isolated posture, no egress and no ingress", network, got)
		}
	}
}

// TestLive_NetworkHostSeesInterfaces pins the opt-in: under "host" the
// container's network view is the daemon host's own — the honest statement
// of what "host" means. The host's listing is read from the test process
// (same machine as the daemon) immediately before and after the container
// read, and the assertion is set-shaped rather than strict equality:
// interfaces come and go on a live host (veth pairs from other containers,
// transient tunnels), so every interface stable across the container read
// must appear in it, and the container must show nothing the host did not
// have around the read.
func TestLive_NetworkHostSeesInterfaces(t *testing.T) {
	sb, ctx := startNetSandbox(t, "host")

	before := hostInterfaces(t)
	got := netInterfaces(t, ctx, sb)
	after := hostInterfaces(t)

	if len(before) == 1 && before[0] == "lo" && len(after) == 1 && after[0] == "lo" {
		t.Skip("the host shows only loopback: host mode would be " +
			"indistinguishable from none on this machine")
	}
	for _, name := range before {
		if slices.Contains(after, name) && !slices.Contains(got, name) {
			t.Errorf("Network \"host\": interface %q was on the host across the "+
				"container read but is missing from the container's view %v", name, got)
		}
	}
	for _, name := range got {
		if !slices.Contains(before, name) && !slices.Contains(after, name) {
			t.Errorf("Network \"host\": container shows interface %q, absent from "+
				"both surrounding host readings %v / %v", name, before, after)
		}
	}
}

// hostInterfaces lists the host's own /sys/class/net, sorted.
func hostInterfaces(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		t.Skipf("cannot read the host's interfaces: %v", err)
	}
	host := make([]string, 0, len(entries))
	for _, e := range entries {
		host = append(host, e.Name())
	}
	sort.Strings(host)
	return host
}
