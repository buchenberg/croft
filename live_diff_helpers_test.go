//go:build linux && !nolive

// Shared helpers for live diff tests (Changes, Diff, ReadFilesAt) against a
// real containerd daemon. Extracted from the Task 0 spike after the golden
// tar was committed; the spike test itself is gone, but its tree/change
// matrix is the canonical change set the live tests exercise.
package croft

import (
	"context"
	"testing"
)

// spikeTree builds the lower tree — the state a live diff test captures. It
// covers the full change matrix: a kept file, a modified file, a deleted
// file, a mode-only file, and an opaque directory (populated, then emptied
// and recreated by spikeChanges).
func spikeTree(t *testing.T, ctx context.Context, sb *ContainerdSandbox) {
	t.Helper()
	mustExec := func(script string) {
		t.Helper()
		res, err := sb.Exec(ctx, ExecRequest{Command: "sh", Args: []string{"-c", script}, Cwd: sb.cfg.workdir()})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exec %q: err=%v exit=%d stderr=%s", script, err, res.ExitCode, res.Stderr)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := sb.WriteFile(ctx, path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	write("base-kept.txt", "kept unchanged\n")
	write("base-modified.txt", "before\n")
	write("base-deleted.txt", "delete me\n")
	write("base-modeonly.txt", "chmod only\n")
	mustExec("mkdir -p opaque-dir && printf 'one\\n' > opaque-dir/a.txt && printf 'two\\n' > opaque-dir/b.txt")
}

// spikeChanges diverges the upper tree from spikeTree's lower: an add, a
// modify, a delete, a mode-only change, a new directory with a file, an
// opaque directory (emptied and recreated), and a symlink.
func spikeChanges(t *testing.T, ctx context.Context, sb *ContainerdSandbox) {
	t.Helper()
	mustExec := func(script string) {
		t.Helper()
		res, err := sb.Exec(ctx, ExecRequest{Command: "sh", Args: []string{"-c", script}, Cwd: sb.cfg.workdir()})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exec %q: err=%v exit=%d stderr=%s", script, err, res.ExitCode, res.Stderr)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := sb.WriteFile(ctx, path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	write("added.txt", "added after capture\n")
	write("base-modified.txt", "after\n")
	mustExec("rm base-deleted.txt")
	mustExec("chmod 600 base-modeonly.txt")
	mustExec("mkdir new-dir && printf 'nested\\n' > new-dir/n.txt")
	mustExec("rm -rf opaque-dir && mkdir opaque-dir && printf 'replaced\\n' > opaque-dir/c.txt")
	mustExec("ln -s base-kept.txt symlink.txt")
}
