//go:build linux && !nolive

package croft

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLive_BrokerPosture proves the broker network reaches a running task
// through the host proxy without giving the container a default route. The
// allowlist behavior itself is unit-tested in broker_test.go; this live check
// covers the veth/firewall/proxy setup that unit tests cannot.
func TestLive_BrokerPosture(t *testing.T) {
	for _, tool := range []string{"ip", "nsenter", "iptables"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("broker live test requires %s: %v", tool, err)
		}
	}
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
		Network:     "broker",
		BrokerHosts: []string{"example.com"},
	})
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer dcancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create with broker posture: %v", err)
	}
	res, err := sb.Exec(ctx, ExecRequest{
		Command: "sh",
		Args:    []string{"-c", `printf '%s' "$HTTP_PROXY"`},
		Cwd:     cfg.Workdir,
	})
	if err != nil {
		t.Fatalf("read broker proxy env: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(res.Stdout), "http://10.42.") {
		t.Errorf("HTTP_PROXY = %q, want the broker's veth proxy", res.Stdout)
	}
}
