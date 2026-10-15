//go:build linux && !nolive

// Live-daemon acceptance tests for the snapshot-native delta: ApplyDelta's
// three-way merge across two sandboxes sharing a base state, the atomic
// conflict refusal, the released-base contract, and blob residue. These
// exercise what the fakes cannot: two real daemon-side diffs between
// committed layers, real overlayfs type replacements, and the application
// scripts (rm/mkdir/chmod/ln/WriteFile) against a real container filesystem.
//
// Run (root, like the rest of the live suite):
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run 'TestLive_ApplyDelta' -timeout 15m ./...
package croft

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// deltaWrite writes a workspace file or fails the test.
func deltaWrite(t *testing.T, ctx context.Context, sb *ContainerdSandbox, path, content string) {
	t.Helper()
	if err := sb.WriteFile(ctx, path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// deltaRead reads a workspace file or fails the test.
func deltaRead(t *testing.T, ctx context.Context, sb *ContainerdSandbox, path string) string {
	t.Helper()
	b, err := sb.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestLive_ApplyDeltaMergesOntoMovedParent is the load-bearing live delta
// test: two sandboxes share a base state, both diverge, and ApplyDelta
// merges theirs onto ours — theirs' add and delete apply, ours' own changes
// survive, and an edit both sides made identically is kept without
// re-application. The workspace was never a git repository at any point.
func TestLive_ApplyDeltaMergesOntoMovedParent(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	// Sandbox A lays the base tree.
	sbA := newLiveSandbox(t, ctx, cfg)
	if err := sbA.Create(ctx); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	deltaWrite(t, ctx, sbA, "base-kept.txt", "kept\n")
	deltaWrite(t, ctx, sbA, "both.txt", "before\n")
	deltaWrite(t, ctx, sbA, "theirs-deletes.txt", "gone\n")
	base := captureLive(t, ctx, sbA)
	if base.BaseKey != "" {
		t.Fatalf("base state BaseKey = %q, want empty (fresh lineage)", base.BaseKey)
	}
	if err := sbA.Apply(ctx, base); err != nil {
		t.Fatalf("Apply A: %v", err)
	}

	// Sandbox B produces theirs on the same base.
	sbB := newLiveSandbox(t, ctx, cfg)
	if err := sbB.Create(ctx); err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if err := sbB.Apply(ctx, base); err != nil {
		t.Fatalf("Apply B: %v", err)
	}
	deltaWrite(t, ctx, sbB, "theirs.txt", "T\n")
	deltaWrite(t, ctx, sbB, "both.txt", "after\n")
	mustExec(t, ctx, sbB, cfg.Workdir, "rm", "theirs-deletes.txt")
	theirs := captureLive(t, ctx, sbB)
	if theirs.BaseKey != base.SnapshotKey {
		t.Fatalf("theirs BaseKey = %q, want the base %q", theirs.BaseKey, base.SnapshotKey)
	}

	// Ours: A moves past the base with its own add and the SAME edit B made.
	deltaWrite(t, ctx, sbA, "ours.txt", "O\n")
	deltaWrite(t, ctx, sbA, "both.txt", "after\n")

	if err := sbA.ApplyDelta(ctx, theirs); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	// The merged tree is base + ours + theirs.
	if got := deltaRead(t, ctx, sbA, "base-kept.txt"); got != "kept\n" {
		t.Errorf("base-kept.txt = %q, want %q", got, "kept\n")
	}
	if got := deltaRead(t, ctx, sbA, "theirs.txt"); got != "T\n" {
		t.Errorf("theirs.txt = %q, want %q (theirs' add applied)", got, "T\n")
	}
	if got := deltaRead(t, ctx, sbA, "both.txt"); got != "after\n" {
		t.Errorf("both.txt = %q, want %q (identical edit kept)", got, "after\n")
	}
	if got := deltaRead(t, ctx, sbA, "ours.txt"); got != "O\n" {
		t.Errorf("ours.txt = %q, want %q (ours survived the merge)", got, "O\n")
	}
	if _, err := sbA.ReadFile(ctx, "theirs-deletes.txt"); err == nil {
		t.Error("theirs-deletes.txt is still readable, want deleted by the merge")
	}

	// ApplyDelta left the lineage base untouched: the next capture still
	// records the last APPLIED state as its base. captureLive (not a bare
	// Capture) so this state's pin is released with the test, like every
	// other live capture.
	after := captureLive(t, ctx, sbA)
	if after.BaseKey != base.SnapshotKey {
		t.Errorf("post-merge BaseKey = %q, want %q (ApplyDelta must not move the base)",
			after.BaseKey, base.SnapshotKey)
	}
}

// TestLive_ApplyDeltaConflictRefusesUntouched pins the atomic refusal on a
// real daemon: divergent edits to the same file produce ErrDeltaConflict
// naming the path, and the workspace keeps ours' version bit-identical.
func TestLive_ApplyDeltaConflictRefusesUntouched(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sbA := newLiveSandbox(t, ctx, cfg)
	if err := sbA.Create(ctx); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	deltaWrite(t, ctx, sbA, "conflict.txt", "before\n")
	base := captureLive(t, ctx, sbA)
	if err := sbA.Apply(ctx, base); err != nil {
		t.Fatalf("Apply A: %v", err)
	}

	sbB := newLiveSandbox(t, ctx, cfg)
	if err := sbB.Create(ctx); err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if err := sbB.Apply(ctx, base); err != nil {
		t.Fatalf("Apply B: %v", err)
	}
	deltaWrite(t, ctx, sbB, "conflict.txt", "theirs\n")
	theirs := captureLive(t, ctx, sbB)

	deltaWrite(t, ctx, sbA, "conflict.txt", "ours\n")

	err := sbA.ApplyDelta(ctx, theirs)
	if !errors.Is(err, ErrDeltaConflict) {
		t.Fatalf("ApplyDelta err = %v, want ErrDeltaConflict", err)
	}
	if !strings.Contains(err.Error(), "conflict.txt") {
		t.Errorf("conflict error %q does not name the conflicting path", err)
	}
	if got := deltaRead(t, ctx, sbA, "conflict.txt"); got != "ours\n" {
		t.Errorf("conflict.txt = %q after refusal, want %q (workspace untouched)", got, "ours\n")
	}
}

// TestLive_ApplyDeltaWorksOnNonGitWorkspace is the headline: the git-era
// ApplyDelta needed delta_patch from a git repository; this one merges a
// plain workspace holding content git never tracked — a retargeted symlink
// and a new directory with a child — with no repository anywhere and no git
// invocation (the image contract is sh/cat/ls/readlink/rm/mkdir/chmod/ln).
func TestLive_ApplyDeltaWorksOnNonGitWorkspace(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sbA := newLiveSandbox(t, ctx, cfg)
	if err := sbA.Create(ctx); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	deltaWrite(t, ctx, sbA, "plain.txt", "P\n")
	mustExec(t, ctx, sbA, cfg.Workdir, "ln", "-s", "plain.txt", "link.txt")
	base := captureLive(t, ctx, sbA)
	if err := sbA.Apply(ctx, base); err != nil {
		t.Fatalf("Apply A: %v", err)
	}

	sbB := newLiveSandbox(t, ctx, cfg)
	if err := sbB.Create(ctx); err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if err := sbB.Apply(ctx, base); err != nil {
		t.Fatalf("Apply B: %v", err)
	}
	deltaWrite(t, ctx, sbB, "other.txt", "O\n")
	// Retarget the symlink (a type-identical modify whose target travels in
	// the blob's Linkname) and add a directory with a child.
	mustExec(t, ctx, sbB, cfg.Workdir, "rm", "link.txt")
	mustExec(t, ctx, sbB, cfg.Workdir, "ln", "-s", "other.txt", "link.txt")
	mustExec(t, ctx, sbB, cfg.Workdir, "mkdir", "newdir")
	deltaWrite(t, ctx, sbB, "newdir/n.txt", "N\n")
	theirs := captureLive(t, ctx, sbB)

	if err := sbA.ApplyDelta(ctx, theirs); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	if got := strings.TrimSpace(mustExec(t, ctx, sbA, cfg.Workdir, "readlink", "link.txt")); got != "other.txt" {
		t.Errorf("link.txt target = %q, want %q (theirs' retarget applied)", got, "other.txt")
	}
	if got := deltaRead(t, ctx, sbA, "other.txt"); got != "O\n" {
		t.Errorf("other.txt = %q, want %q", got, "O\n")
	}
	if got := deltaRead(t, ctx, sbA, "newdir/n.txt"); got != "N\n" {
		t.Errorf("newdir/n.txt = %q, want %q (dir + child applied)", got, "N\n")
	}
	if got := deltaRead(t, ctx, sbA, "plain.txt"); got != "P\n" {
		t.Errorf("plain.txt = %q, want %q (untouched)", got, "P\n")
	}
}

// TestLive_ApplyDeltaAfterBaseReleasedFails pins the base-side half of the
// unavailable-state contract. ReleaseState drops the base's pin, but the
// layer itself survives until the daemon's GC runs — and here it is also a
// parent of the sandbox's own chain, so waiting for its disappearance would
// be nondeterministic. As in the phase 2 diff tests, a ghost base key — a
// snapshot key the daemon has never seen — is exactly what a
// released-and-GC'd base looks like to the merge; ReleaseState is still
// exercised (its return is checked).
func TestLive_ApplyDeltaAfterBaseReleasedFails(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	deltaWrite(t, ctx, sb, "f.txt", "F\n")
	base := captureLive(t, ctx, sb)
	if err := sb.Apply(ctx, base); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	deltaWrite(t, ctx, sb, "g.txt", "G\n")
	child := captureLive(t, ctx, sb)
	if child.BaseKey != base.SnapshotKey {
		t.Fatalf("child BaseKey = %q, want %q", child.BaseKey, base.SnapshotKey)
	}

	// Exercise the release path, then assert against the ghost.
	if err := sb.ReleaseState(ctx, base); err != nil {
		t.Fatalf("ReleaseState: %v", err)
	}
	ghosted := WorkspaceState{SnapshotKey: child.SnapshotKey, BaseKey: "croft/ghost/committed/1", Image: child.Image}
	err := sb.ApplyDelta(ctx, ghosted)
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("ApplyDelta with a released base: err = %v, want ErrStateUnavailable", err)
	}

	// The refusal left the workspace usable and untouched.
	if got := deltaRead(t, ctx, sb, "g.txt"); got != "G\n" {
		t.Errorf("g.txt = %q after refusal, want %q", got, "G\n")
	}
}

// TestLive_ApplyDeltaBlobLeavesNoContentResidue pins the house blob-lease
// hygiene across both merge legs: after a successful ApplyDelta, no NEW
// croft-diff lease and no NEW diffview snapshot remain in the namespace.
// The blobs themselves are deliberately left for the daemon's GC once
// unpinned — eager deletion would break concurrent callers that computed
// the same digest (phase 2's rule); the leases and views are what croft
// owns, and they must be gone. Pre-existing leftovers from a crashed run
// are excluded by comparing before/after sets rather than requiring empty.
func TestLive_ApplyDeltaBlobLeavesNoContentResidue(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	c := dial(t, cfg.Addr)
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close client: %v", err)
		}
	}()
	lctx := nsCtx(ctx, cfg.Namespace)
	diffLeaseFilter := fmt.Sprintf("labels.%q==%q", sandboxLeaseLabel, "diff")

	leasesBefore := map[string]bool{}
	list, err := c.LeasesService().List(lctx, diffLeaseFilter)
	if err != nil {
		t.Fatalf("list diff leases before: %v", err)
	}
	for _, l := range list {
		leasesBefore[l.ID] = true
	}
	viewsBefore := map[string]bool{}
	for k := range liveSnapshotKeys(t, lctx, c, cfg.Snapshotter) {
		if strings.Contains(k, "/diffview/") {
			viewsBefore[k] = true
		}
	}

	// A minimal successful merge.
	sbA := newLiveSandbox(t, ctx, cfg)
	if err := sbA.Create(ctx); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	deltaWrite(t, ctx, sbA, "f.txt", "before\n")
	base := captureLive(t, ctx, sbA)
	if err := sbA.Apply(ctx, base); err != nil {
		t.Fatalf("Apply A: %v", err)
	}

	sbB := newLiveSandbox(t, ctx, cfg)
	if err := sbB.Create(ctx); err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if err := sbB.Apply(ctx, base); err != nil {
		t.Fatalf("Apply B: %v", err)
	}
	deltaWrite(t, ctx, sbB, "f.txt", "after\n")
	theirs := captureLive(t, ctx, sbB)

	if err := sbA.ApplyDelta(ctx, theirs); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if got := deltaRead(t, ctx, sbA, "f.txt"); got != "after\n" {
		t.Fatalf("f.txt = %q, want %q (merge did not apply)", got, "after\n")
	}

	list, err = c.LeasesService().List(lctx, diffLeaseFilter)
	if err != nil {
		t.Fatalf("list diff leases after: %v", err)
	}
	for _, l := range list {
		if !leasesBefore[l.ID] {
			t.Errorf("diff lease %s survived ApplyDelta", l.ID)
		}
	}
	for k := range liveSnapshotKeys(t, lctx, c, cfg.Snapshotter) {
		if strings.Contains(k, "/diffview/") && !viewsBefore[k] {
			t.Errorf("diffview snapshot %s survived ApplyDelta", k)
		}
	}
}
