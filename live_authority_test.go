//go:build linux && !nolive

package croft

import (
	"context"
	"testing"
	"time"
)

// TestLive_WritableRootShaping exercises the ADR 0001 A′ shape against a real
// daemon: the workspace root is read-only, a granted subtree is writable, and
// the launch probe refuses an unmasked workspace before the caller sees it.
func TestLive_WritableRootShaping(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	c := dial(t, cfg.Addr)
	ensureNamespace(t, ctx, c, cfg.Namespace)
	if err := c.Close(); err != nil {
		t.Logf("close setup client: %v", err)
	}

	sb := New(Config{
		Address:       cfg.Addr,
		Namespace:     cfg.Namespace,
		Image:         cfg.Image,
		Snapshotter:   cfg.Snapshotter,
		Workdir:       cfg.Workdir,
		FIFODir:       t.TempDir(),
		WritableRoots: []string{cfg.Workdir + "/out"},
	})
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer dcancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create with writable-root shaping: %v", err)
	}

	outside := sb.resolve(".croft-live-outside")
	res, err := sb.Exec(ctx, ExecRequest{Command: "touch", Args: []string{outside}, Cwd: cfg.Workdir})
	if err != nil {
		t.Fatalf("touch outside grant: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("touch outside the granted root succeeded; the workspace mask is not enforced")
	}

	inside := sb.resolve("out/.croft-live-inside")
	res, err = sb.Exec(ctx, ExecRequest{Command: "touch", Args: []string{inside}, Cwd: cfg.Workdir})
	if err != nil {
		t.Fatalf("touch inside grant: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("touch inside the granted root failed (exit %d: %s)", res.ExitCode, res.Stderr)
	}
	// The shaped workspace must still capture normally.
	if _, err := sb.Capture(ctx); err != nil {
		t.Errorf("Capture after shaping: %v", err)
	}
}
