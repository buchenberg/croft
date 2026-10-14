//go:build linux && !nolive

// Live-daemon acceptance tests for the snapshot-native diff machinery:
// Changes, Diff, ReadFilesAt, and the released-state contract. These run
// against a real containerd daemon's diff service and exercise the full
// production path — diffBlob, parseChangeTar, reader tasks, and the unified
// renderer — that the unit suite can only test against fakes.
//
// Run (root, like the rest of the live suite):
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run 'TestLive_(Changes|Diff|ReadFilesAt)' ./...
package croft

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// changeMap indexes a change list by path for lookup assertions.
func changeMap(changes []Change) map[string]Change {
	m := make(map[string]Change, len(changes))
	for _, c := range changes {
		m[c.Path] = c
	}
	return m
}

// TestLive_ChangesReportsAddModifyDelete is the load-bearing live diff test:
// the spike's full change matrix (add, modify, delete, mode-only, new
// directory, opaque directory, symlink) flows through the production Changes
// API against a real daemon-side diff service.
func TestLive_ChangesReportsAddModifyDelete(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spikeTree(t, ctx, sb)
	state := captureLive(t, ctx, sb)
	spikeChanges(t, ctx, sb)

	changes, err := sb.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	// Sorted by path.
	for i := 1; i < len(changes); i++ {
		if changes[i-1].Path >= changes[i].Path {
			t.Errorf("changes not sorted: %q >= %q at index %d", changes[i-1].Path, changes[i].Path, i)
		}
	}

	m := changeMap(changes)

	// Adds: new files absent from the lower tree.
	for _, path := range []string{"added.txt", "new-dir/n.txt", "opaque-dir/c.txt", "symlink.txt"} {
		if got := m[path].Kind; got != ChangeAdd {
			t.Errorf("%s: kind = %v, want add", path, got)
		}
	}

	// Deletes: whiteouts for files present in the lower tree, absent in upper.
	for _, path := range []string{"base-deleted.txt", "opaque-dir/a.txt", "opaque-dir/b.txt"} {
		if got := m[path].Kind; got != ChangeDelete {
			t.Errorf("%s: kind = %v, want delete", path, got)
		}
	}

	// Modifies: present in both, content or mode differs.
	for _, path := range []string{"base-modified.txt", "base-modeonly.txt"} {
		if got := m[path].Kind; got != ChangeModify {
			t.Errorf("%s: kind = %v, want modify", path, got)
		}
	}

	// New directory: an add with IsDir.
	if got := m["new-dir"]; got.Kind != ChangeAdd || !got.IsDir {
		t.Errorf("new-dir: kind = %v isDir = %v, want add + dir", got.Kind, got.IsDir)
	}

	// The opaque directory itself exists in both trees: a modify (its
	// contents were replaced wholesale, reported via per-file whiteouts).
	if got := m["opaque-dir"]; got.Kind != ChangeModify || !got.IsDir {
		t.Errorf("opaque-dir: kind = %v isDir = %v, want modify + dir", got.Kind, got.IsDir)
	}

	// The unchanged file must not appear.
	if _, ok := m["base-kept.txt"]; ok {
		t.Error("base-kept.txt must not be in the change list (unchanged)")
	}

	t.Logf("Changes reported %d changes across the spike matrix", len(changes))
}

// TestLive_DiffRendersUnifiedText drives the production Diff through the
// spike's change matrix and asserts the unified-diff structure: /dev/null
// sides for adds and deletes, -old/+new lines for modifies, and an
// old mode/new mode pair for the mode-only change.
func TestLive_DiffRendersUnifiedText(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spikeTree(t, ctx, sb)
	state := captureLive(t, ctx, sb)
	spikeChanges(t, ctx, sb)

	diff, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == "" {
		t.Fatal("Diff returned empty output for a non-empty change set")
	}

	// Add: /dev/null as the a side.
	if !strings.Contains(diff, "--- /dev/null\n+++ b/added.txt") {
		t.Error("Diff: missing add section for added.txt (/dev/null as a side)")
	}

	// Modify: -before / +after.
	if !strings.Contains(diff, "--- a/base-modified.txt\n+++ b/base-modified.txt") {
		t.Error("Diff: missing modify headers for base-modified.txt")
	}
	if !strings.Contains(diff, "-before\n") {
		t.Error("Diff: missing '-before' line for base-modified.txt")
	}
	if !strings.Contains(diff, "+after\n") {
		t.Error("Diff: missing '+after' line for base-modified.txt")
	}

	// Delete: /dev/null as the b side.
	if !strings.Contains(diff, "--- a/base-deleted.txt\n+++ /dev/null") {
		t.Error("Diff: missing delete section for base-deleted.txt (/dev/null as b side)")
	}

	// Mode-only: old mode / new mode pair (0644 → 0600).
	if !strings.Contains(diff, "old mode 0644\nnew mode 0600") {
		t.Error("Diff: missing mode-only pair for base-modeonly.txt (0644 → 0600)")
	}

	// Symlink: rendered as a target line (the link points at base-kept.txt).
	if !strings.Contains(diff, "b/symlink.txt") {
		t.Error("Diff: missing symlink section for symlink.txt")
	}

	t.Logf("Diff rendered %d bytes of unified output", len(diff))
}

// TestLive_ChangesOnReleasedStateFailsWithErrStateUnavailable pins the
// released-state contract: once a state's layer is gone, Changes and Diff
// fail with ErrStateUnavailable. ReleaseState drops the pin; the daemon's GC
// eventually reclaims the layer. To test the post-GC contract
// deterministically (without waiting for GC or fighting snapshot children),
// a ghost state — a snapshot key the daemon has never seen — is exactly what
// a released-and-GC'd state looks like to Changes/Diff.
func TestLive_ChangesOnReleasedStateFailsWithErrStateUnavailable(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spikeTree(t, ctx, sb)
	state := captureLive(t, ctx, sb)

	// Release the state's pin — this exercises the ReleaseState path.
	if err := sb.ReleaseState(ctx, state); err != nil {
		t.Fatalf("ReleaseState: %v", err)
	}

	// A ghost state simulates the post-GC state: the layer does not exist.
	ghost := WorkspaceState{SnapshotKey: "croft/ghost/committed/1"}

	_, err := sb.Changes(ctx, ghost)
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("Changes on unavailable state: err = %v, want ErrStateUnavailable", err)
	}

	_, err = sb.Diff(ctx, ghost)
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("Diff on unavailable state: err = %v, want ErrStateUnavailable", err)
	}
}

// TestLive_ReadFilesAt verifies that pre-images are read from the state's
// committed layer, not the current workspace: base-modified.txt's pre-image
// is "before\n" even after the workspace has "after\n".
func TestLive_ReadFilesAt(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spikeTree(t, ctx, sb)
	state := captureLive(t, ctx, sb)
	spikeChanges(t, ctx, sb)

	files, err := sb.ReadFilesAt(ctx, state, []string{"base-kept.txt", "base-modified.txt"})
	if err != nil {
		t.Fatalf("ReadFilesAt: %v", err)
	}

	if got, want := string(files["base-kept.txt"]), "kept unchanged\n"; got != want {
		t.Errorf("base-kept.txt pre-image = %q, want %q", got, want)
	}
	if got, want := string(files["base-modified.txt"]), "before\n"; got != want {
		t.Errorf("base-modified.txt pre-image = %q, want %q (state, not current)", got, want)
	}

	// A path absent from the state fails the call.
	_, err = sb.ReadFilesAt(ctx, state, []string{"never-existed.txt"})
	if err == nil {
		t.Error("ReadFilesAt on a missing path: want an error, got nil")
	}
}

// TestLive_DiffConcurrentOperations is the race smoke test: Changes, Diff,
// and ReadFilesAt all take the shared operation lock and can run
// concurrently; the race detector catches data races, and the lock prevents
// lifecycle interleaving. Run under `go test -race`.
func TestLive_DiffConcurrentOperations(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spikeTree(t, ctx, sb)
	state := captureLive(t, ctx, sb)
	spikeChanges(t, ctx, sb)

	const goroutines = 3
	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errs[0] = sb.Changes(ctx, state)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errs[1] = sb.Diff(ctx, state)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errs[2] = sb.ReadFilesAt(ctx, state, []string{"base-kept.txt", "base-modified.txt"})
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent op %d: %v", i, err)
		}
	}
}
