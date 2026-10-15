package croft

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Delta merge fixtures. The algebra only reads Path, Kind, Mode, IsDir, and
// the content maps, so fixtures stay minimal; Size is irrelevant to merging
// (identical bytes imply identical sizes).

func regChange(path string, kind ChangeKind, perm fs.FileMode) Change {
	return Change{Path: path, Kind: kind, Mode: perm}
}

func dirChange(path string, kind ChangeKind, perm fs.FileMode) Change {
	return Change{Path: path, Kind: kind, IsDir: true, Mode: perm | fs.ModeDir}
}

func linkChange(path string, kind ChangeKind) Change {
	return Change{Path: path, Kind: kind, Mode: 0o777 | fs.ModeSymlink}
}

func del(path string) Change {
	return Change{Path: path, Kind: ChangeDelete}
}

func contents(pairs ...string) map[string][]byte {
	m := make(map[string][]byte, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = []byte(pairs[i+1])
	}
	return m
}

// TestMergeChanges is the full merge-class matrix from the delta plan's §1.2:
// every combination class, directories, mode-only changes, type changes, and
// the delete-vs-underneath collision the per-path walk cannot see.
func TestMergeChanges(t *testing.T) {
	tests := []struct {
		name          string
		theirs, ours  []Change
		theirsContent map[string][]byte
		oursContent   map[string][]byte
		wantApply     []Change
		wantConflicts []string // paths, sorted
	}{
		{
			name:      "empty both sides",
			wantApply: nil,
		},
		{
			name:      "theirs empty, ours moved: nothing to apply",
			ours:      []Change{regChange("f.txt", ChangeModify, 0o644)},
			wantApply: nil,
		},
		{
			name:          "ours empty: everything from theirs applies",
			theirs:        []Change{regChange("a.txt", ChangeAdd, 0o644), regChange("d.txt", ChangeDelete, 0), regChange("m.txt", ChangeModify, 0o600)},
			theirsContent: contents("a.txt", "A", "m.txt", "M"),
			wantApply:     []Change{regChange("a.txt", ChangeAdd, 0o644), regChange("d.txt", ChangeDelete, 0), regChange("m.txt", ChangeModify, 0o600)},
		},
		{
			name:          "disjoint paths: theirs applies, ours keeps",
			theirs:        []Change{regChange("theirs.txt", ChangeModify, 0o644)},
			ours:          []Change{regChange("ours.txt", ChangeModify, 0o644)},
			theirsContent: contents("theirs.txt", "T"),
			oursContent:   contents("ours.txt", "O"),
			wantApply:     []Change{regChange("theirs.txt", ChangeModify, 0o644)},
		},
		{
			name:          "both modified identically: keep, no apply",
			theirs:        []Change{regChange("f.txt", ChangeModify, 0o644)},
			ours:          []Change{regChange("f.txt", ChangeModify, 0o644)},
			theirsContent: contents("f.txt", "same"),
			oursContent:   contents("f.txt", "same"),
			wantApply:     nil,
		},
		{
			name:          "both modified divergently: conflict",
			theirs:        []Change{regChange("f.txt", ChangeModify, 0o644)},
			ours:          []Change{regChange("f.txt", ChangeModify, 0o644)},
			theirsContent: contents("f.txt", "their edit"),
			oursContent:   contents("f.txt", "our edit"),
			wantApply:     nil,
			wantConflicts: []string{"f.txt"},
		},
		{
			name:          "same bytes, different mode: conflict",
			theirs:        []Change{regChange("f.txt", ChangeModify, 0o600)},
			ours:          []Change{regChange("f.txt", ChangeModify, 0o644)},
			theirsContent: contents("f.txt", "same"),
			oursContent:   contents("f.txt", "same"),
			wantApply:     nil,
			wantConflicts: []string{"f.txt"},
		},
		{
			name:          "both empty files, same mode: keep (nil == empty content)",
			theirs:        []Change{regChange("e.txt", ChangeModify, 0o644)},
			ours:          []Change{regChange("e.txt", ChangeModify, 0o644)},
			theirsContent: nil, // parser omits empty bodies
			oursContent:   nil,
			wantApply:     nil,
		},
		{
			name:          "empty vs non-empty: conflict",
			theirs:        []Change{regChange("e.txt", ChangeModify, 0o644)},
			ours:          []Change{regChange("e.txt", ChangeModify, 0o644)},
			theirsContent: nil,
			oursContent:   contents("e.txt", "not empty"),
			wantApply:     nil,
			wantConflicts: []string{"e.txt"},
		},
		{
			name:      "both deleted: keep",
			theirs:    []Change{del("gone.txt")},
			ours:      []Change{del("gone.txt")},
			wantApply: nil,
		},
		{
			name:          "theirs modifies, ours deletes: conflict",
			theirs:        []Change{regChange("f.txt", ChangeModify, 0o644)},
			ours:          []Change{del("f.txt")},
			theirsContent: contents("f.txt", "T"),
			wantApply:     nil,
			wantConflicts: []string{"f.txt"},
		},
		{
			name:          "theirs deletes, ours modifies: conflict",
			theirs:        []Change{del("f.txt")},
			ours:          []Change{regChange("f.txt", ChangeModify, 0o644)},
			oursContent:   contents("f.txt", "O"),
			wantApply:     nil,
			wantConflicts: []string{"f.txt"},
		},
		{
			name:          "type change regular vs symlink: conflict via mode bits",
			theirs:        []Change{regChange("p", ChangeModify, 0o644)},
			ours:          []Change{linkChange("p", ChangeModify)},
			theirsContent: contents("p", "content"),
			oursContent:   contents("p", "target"),
			wantApply:     nil,
			wantConflicts: []string{"p"},
		},
		{
			name:          "both retargeted the symlink identically: keep",
			theirs:        []Change{linkChange("l", ChangeModify)},
			ours:          []Change{linkChange("l", ChangeModify)},
			theirsContent: contents("l", "newtarget"),
			oursContent:   contents("l", "newtarget"),
			wantApply:     nil,
		},
		{
			name:          "symlink retargeted divergently: conflict",
			theirs:        []Change{linkChange("l", ChangeModify)},
			ours:          []Change{linkChange("l", ChangeModify)},
			theirsContent: contents("l", "their-target"),
			oursContent:   contents("l", "our-target"),
			wantApply:     nil,
			wantConflicts: []string{"l"},
		},
		{
			name:      "both changed a directory identically (mode): keep",
			theirs:    []Change{dirChange("d", ChangeModify, 0o755)},
			ours:      []Change{dirChange("d", ChangeModify, 0o755)},
			wantApply: nil,
		},
		{
			name:          "directory mode disagreement: conflict",
			theirs:        []Change{dirChange("d", ChangeModify, 0o700)},
			ours:          []Change{dirChange("d", ChangeModify, 0o755)},
			wantApply:     nil,
			wantConflicts: []string{"d"},
		},
		{
			name:      "theirs adds a directory: applies",
			theirs:    []Change{dirChange("newdir", ChangeAdd, 0o755)},
			wantApply: []Change{dirChange("newdir", ChangeAdd, 0o755)},
		},
		{
			name:        "theirs deletes a directory, ours untouched inside: applies",
			theirs:      []Change{del("olddir"), del("olddir/a.txt")},
			ours:        []Change{regChange("unrelated.txt", ChangeModify, 0o644)},
			oursContent: contents("unrelated.txt", "O"),
			wantApply:   []Change{del("olddir"), del("olddir/a.txt")},
		},
		{
			name:      "theirs deletes a directory, ours added a file inside: conflict",
			theirs:    []Change{del("d")},
			ours:      []Change{regChange("d/new.txt", ChangeAdd, 0o644)},
			wantApply: nil,
			// the conflict names the theirs-side entry: the incoming delete
			wantConflicts: []string{"d"},
		},
		{
			name:          "ours deletes a directory, theirs added a file inside: conflict",
			theirs:        []Change{regChange("d/new.txt", ChangeAdd, 0o644)},
			ours:          []Change{del("d")},
			theirsContent: contents("d/new.txt", "N"),
			wantApply:     nil,
			wantConflicts: []string{"d/new.txt"},
		},
		{
			name:          "theirs replaces a path with a file, ours added a child under it: conflict",
			theirs:        []Change{regChange("p", ChangeModify, 0o644)},
			ours:          []Change{regChange("p/child", ChangeAdd, 0o644)},
			theirsContent: contents("p", "file now"),
			oursContent:   contents("p/child", "C"),
			wantApply:     nil,
			wantConflicts: []string{"p"},
		},
		{
			name:          "ours replaced a path with a file, theirs added a child under it: conflict",
			theirs:        []Change{regChange("p/child", ChangeAdd, 0o644)},
			ours:          []Change{regChange("p", ChangeModify, 0o644)},
			theirsContent: contents("p/child", "C"),
			oursContent:   contents("p", "file now"),
			wantApply:     nil,
			wantConflicts: []string{"p/child"},
		},
		{
			name:          "theirs added under a directory ours modified: compatible",
			theirs:        []Change{regChange("d/new.txt", ChangeAdd, 0o644)},
			ours:          []Change{dirChange("d", ChangeModify, 0o700)},
			theirsContent: contents("d/new.txt", "N"),
			wantApply:     []Change{regChange("d/new.txt", ChangeAdd, 0o644)},
		},
		{
			name:      "deletes underneath a rigid entry are compatible",
			theirs:    []Change{del("d/x")},
			ours:      []Change{del("d")},
			wantApply: []Change{del("d/x")},
		},
		{
			name:          "collision demotes only the collided delete from apply",
			theirs:        []Change{del("d"), regChange("safe.txt", ChangeAdd, 0o644)},
			ours:          []Change{regChange("d/f.txt", ChangeModify, 0o644)},
			theirsContent: contents("safe.txt", "S"),
			oursContent:   contents("d/f.txt", "O"),
			wantApply:     []Change{regChange("safe.txt", ChangeAdd, 0o644)},
			wantConflicts: []string{"d"},
		},
		{
			name:        "prefix lookalike is not underneath: no collision",
			theirs:      []Change{del("dir")},
			ours:        []Change{regChange("dir2/f.txt", ChangeAdd, 0o644), regChange("directory", ChangeModify, 0o644)},
			oursContent: contents("dir2/f.txt", "O", "directory", "O"),
			wantApply:   []Change{del("dir")},
		},
		{
			name:      "both deleted a child, theirs deleted the parent, ours kept a sibling: no conflict",
			theirs:    []Change{del("d"), del("d/a.txt"), del("d/b.txt")},
			ours:      []Change{del("d/a.txt")},
			wantApply: []Change{del("d"), del("d/b.txt")},
		},
		{
			name:          "mode-only change from theirs applies with content",
			theirs:        []Change{regChange("f.txt", ChangeModify, 0o600)},
			theirsContent: contents("f.txt", "unchanged body"),
			wantApply:     []Change{regChange("f.txt", ChangeModify, 0o600)},
		},
		{
			name:          "conflicts sort and dedupe across rules",
			theirs:        []Change{regChange("a.txt", ChangeModify, 0o644), del("b"), regChange("z.txt", ChangeModify, 0o644)},
			ours:          []Change{regChange("a.txt", ChangeModify, 0o644), regChange("b/under.txt", ChangeAdd, 0o644), del("z.txt")},
			theirsContent: contents("a.txt", "T", "z.txt", "T"),
			oursContent:   contents("a.txt", "O", "b/under.txt", "O"),
			wantApply:     nil,
			wantConflicts: []string{"a.txt", "b", "z.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apply, conflicts := mergeChanges(tt.theirs, tt.ours, tt.theirsContent, tt.oursContent)

			assertChanges(t, apply, tt.wantApply)

			var gotConflicts []string
			for _, c := range conflicts {
				gotConflicts = append(gotConflicts, c.Path)
			}
			if fmt.Sprint(gotConflicts) != fmt.Sprint(tt.wantConflicts) {
				t.Errorf("conflict paths = %v, want %v", gotConflicts, tt.wantConflicts)
			}
			// Conflicts are theirs-side entries by contract.
			for _, c := range conflicts {
				if !containsChangePath(tt.theirs, c.Path) {
					t.Errorf("conflict %q is not a theirs-side entry", c.Path)
				}
			}
			// No path is both applied and conflicted.
			for _, a := range apply {
				for _, c := range conflicts {
					if a.Path == c.Path {
						t.Errorf("path %q is in both apply and conflicts", a.Path)
					}
				}
			}
		})
	}
}

func containsChangePath(list []Change, path string) bool {
	for _, c := range list {
		if c.Path == path {
			return true
		}
	}
	return false
}

// TestForEachUnder pins the range-scan bounds: strictly-underneath paths only,
// sorted order, early stop.
func TestForEachUnder(t *testing.T) {
	list := []Change{
		{Path: "a"}, {Path: "d"}, {Path: "d/a"}, {Path: "d/b/c"}, {Path: "d0"},
		{Path: "d-"}, {Path: "dir"}, {Path: "e"},
	}
	var got []string
	forEachUnder(list, "d", func(c Change) bool {
		got = append(got, c.Path)
		return true
	})
	want := "[d/a d/b/c]"
	if fmt.Sprint(got) != want {
		t.Errorf("forEachUnder(d) = %v, want %s", got, want)
	}

	// Early stop after the first match.
	count := 0
	forEachUnder(list, "d", func(Change) bool {
		count++
		return false
	})
	if count != 1 {
		t.Errorf("early stop visited %d entries, want 1", count)
	}

	// A path with nothing under it visits nothing.
	got = nil
	forEachUnder(list, "e", func(c Change) bool {
		got = append(got, c.Path)
		return true
	})
	if len(got) != 0 {
		t.Errorf("forEachUnder(e) = %v, want empty", got)
	}
}

// TestPlanDelta pins the mutation sequence: grouping order (removes, dirs,
// writes, symlinks), preserved path order within groups, content and mode
// pass-through, and the pre-mutation refusals.
func TestPlanDelta(t *testing.T) {
	t.Run("ordered groups and payload", func(t *testing.T) {
		apply := []Change{
			dirChange("a", ChangeAdd, 0o750),
			regChange("a/new.txt", ChangeAdd, 0o640),
			regChange("b/setuid", ChangeModify, 0o755|fs.ModeSetuid),
			regChange("e/empty", ChangeAdd, 0o644),
			del("gone.txt"),
			linkChange("l", ChangeAdd),
		}
		content := contents("a/new.txt", "N", "l", "target", "b/setuid", "S")

		ops, err := planDelta(apply, content)
		if err != nil {
			t.Fatalf("planDelta: %v", err)
		}

		want := []deltaOp{
			{kind: opRemove, path: "gone.txt"},
			{kind: opMkdir, path: "a", mode: 0o750 | fs.ModeDir},
			{kind: opWrite, path: "a/new.txt", content: []byte("N"), mode: 0o640},
			{kind: opWrite, path: "b/setuid", content: []byte("S"), mode: 0o755 | fs.ModeSetuid},
			{kind: opWrite, path: "e/empty", content: nil, mode: 0o644},
			{kind: opSymlink, path: "l", target: "target"},
		}
		if !reflect.DeepEqual(ops, want) {
			t.Errorf("planDelta:\n got: %+v\nwant: %+v", ops, want)
		}
	})

	t.Run("same-path delete then recreate orders remove first", func(t *testing.T) {
		// A file→directory replacement arrives as a delete and a dir add on
		// the same path; the remove must precede the mkdir.
		apply := []Change{del("p"), dirChange("p", ChangeAdd, 0o755)}
		ops, err := planDelta(apply, nil)
		if err != nil {
			t.Fatalf("planDelta: %v", err)
		}
		if len(ops) != 2 || ops[0].kind != opRemove || ops[1].kind != opMkdir {
			t.Fatalf("ops = %+v, want [remove p, mkdir p]", ops)
		}
	})

	t.Run("special file refuses before any mutation", func(t *testing.T) {
		apply := []Change{
			regChange("ok.txt", ChangeAdd, 0o644),
			{Path: "pipe", Kind: ChangeAdd, Mode: 0o644 | fs.ModeNamedPipe},
		}
		_, err := planDelta(apply, contents("ok.txt", "x"))
		if err == nil || !strings.Contains(err.Error(), "cannot apply special file pipe") {
			t.Fatalf("err = %v, want a cannot-apply-special-file refusal", err)
		}
	})

	t.Run("symlink without a target refuses", func(t *testing.T) {
		apply := []Change{linkChange("l", ChangeAdd)}
		_, err := planDelta(apply, nil)
		if err == nil || !strings.Contains(err.Error(), "no target in the delta blob") {
			t.Fatalf("err = %v, want a missing-target refusal", err)
		}
	})

	t.Run("empty apply set plans nothing", func(t *testing.T) {
		ops, err := planDelta(nil, nil)
		if err != nil {
			t.Fatalf("planDelta: %v", err)
		}
		if len(ops) != 0 {
			t.Errorf("ops = %+v, want empty", ops)
		}
	})
}

// TestApplyDelta_RejectsZeroState pins the zero-value guard, matching
// Apply/ReleaseState.
func TestApplyDelta_RejectsZeroState(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	err := sb.ApplyDelta(context.Background(), WorkspaceState{})
	if err == nil || !strings.Contains(err.Error(), "no snapshot key") {
		t.Fatalf("err = %v, want a no-snapshot-key error", err)
	}
}

// TestApplyDelta_RejectsStateWithoutBaseKey pins the fresh-lineage refusal:
// a state captured from a sandbox that never Applied has no merge base, and
// ApplyDelta says so instead of guessing one.
func TestApplyDelta_RejectsStateWithoutBaseKey(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if state.BaseKey != "" {
		t.Fatalf("precondition: BaseKey = %q, want empty", state.BaseKey)
	}

	err = sb.ApplyDelta(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "no base key") {
		t.Fatalf("err = %v, want a no-base-key error", err)
	}
}

// TestApplyDelta_BeforeCreate pins the guard: no active layer to merge onto.
func TestApplyDelta_BeforeCreate(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	ws := WorkspaceState{SnapshotKey: "croft/x/committed/1", BaseKey: "croft/x/committed/0"}
	err := sb.ApplyDelta(context.Background(), ws)
	if err == nil || !strings.Contains(err.Error(), "ApplyDelta before Create") {
		t.Fatalf("err = %v, want an ApplyDelta-before-Create error", err)
	}
}

// TestApplyDelta_NoDiffService pins the nil-collaborator error, the same
// fail-loud convention as Changes.
func TestApplyDelta_NoDiffService(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ws := WorkspaceState{SnapshotKey: "croft/x/committed/1", BaseKey: "croft/x/committed/0"}
	err := sb.ApplyDelta(ctx, ws)
	if err == nil || !strings.Contains(err.Error(), "diff service not configured") {
		t.Fatalf("err = %v, want a diff-service-not-configured error", err)
	}
}

// TestApplyDelta_ReleasedBaseIsUnavailable pins the base-side half of the
// ErrStateUnavailable contract: a base layer the snapshotter no longer has
// (released and GC'd) fails the merge before any mutation, and the diff
// machinery leaves no view or lease behind.
func TestApplyDelta_ReleasedBaseIsUnavailable(t *testing.T) {
	sb, snap, _, fl, _ := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	ws := WorkspaceState{SnapshotKey: state.SnapshotKey, BaseKey: "croft/ghost/committed/1"}
	err = sb.ApplyDelta(ctx, ws)
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("err = %v, want ErrStateUnavailable", err)
	}
	if keys := viewKeys(snap); len(keys) != 0 {
		t.Errorf("diff views leaked: %v", keys)
	}
	// The per-call diff lease was deleted on the failure path (the sandbox's
	// own lease is outlived by design — count only croft-diff- leases).
	diffIDs := diffLeaseIDs(fl)
	if len(diffIDs) == 0 {
		t.Fatal("no diff lease was created")
	}
	fl.mu.Lock()
	deleted := append([]string(nil), fl.deleted...)
	fl.mu.Unlock()
	for _, id := range diffIDs {
		found := false
		for _, d := range deleted {
			if d == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("diff lease %s was not deleted after the failed merge", id)
		}
	}
}

// TestApplyDelta_ReleasedStateIsUnavailable pins the state-side half: the
// base exists, the state's layer does not.
func TestApplyDelta_ReleasedStateIsUnavailable(t *testing.T) {
	sb, snap, _, _, _ := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	base, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := sb.Apply(ctx, base); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	ws := WorkspaceState{SnapshotKey: "croft/ghost/committed/9", BaseKey: base.SnapshotKey}
	err = sb.ApplyDelta(ctx, ws)
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("err = %v, want ErrStateUnavailable", err)
	}
	if keys := viewKeys(snap); len(keys) != 0 {
		t.Errorf("diff views leaked: %v", keys)
	}
}

// deltaTheirsKey is the seeded committed layer standing in for another
// sandbox's captured state in the fake ApplyDelta tests.
const deltaTheirsKey = "croft/theirs/committed/1"

// deltaTestSandbox primes a sandbox that has Applied a base state, so
// ApplyDelta has a merge base, and seeds the theirs layer. The returned
// state is the base; deltaState builds the incoming state on top of it.
func deltaTestSandbox(t *testing.T) (*ContainerdSandbox, *fakeSnapshotter, *fakeTasks, *fakeLeases, *fakeDiffService, WorkspaceState) {
	t.Helper()
	sb, snap, tasks, fl, fds := changesTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	base, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := sb.Apply(ctx, base); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	snap.seed(deltaTheirsKey)
	return sb, snap, tasks, fl, fds, base
}

// deltaState is the incoming state: theirs' seeded layer over the base.
func deltaState(base WorkspaceState) WorkspaceState {
	return WorkspaceState{SnapshotKey: deltaTheirsKey, BaseKey: base.SnapshotKey, Image: base.Image}
}

// decodeDeltaWrites extracts path → decoded content from WriteFile execs
// (the set -e/base64 script), concatenating chunks across multi-exec writes.
func decodeDeltaWrites(t *testing.T, reqs []ExecRequest) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range reqs {
		if r.Command != "sh" || len(r.Args) < 6 || !strings.Contains(r.Args[1], "base64 -d") {
			continue
		}
		path := r.Args[3]
		dec, err := base64.StdEncoding.DecodeString(strings.Join(r.Args[6:], ""))
		if err != nil {
			t.Fatalf("decode write payload for %s: %v", path, err)
		}
		out[path] += string(dec)
	}
	return out
}

// scriptPaths lists the resolved-path arguments ($1) of every exec that ran
// the given script, in order.
func scriptPaths(reqs []ExecRequest, script string) []string {
	var out []string
	for _, r := range reqs {
		if r.Command == "sh" && len(r.Args) >= 4 && r.Args[1] == script {
			out = append(out, r.Args[3])
		}
	}
	return out
}

// TestApplyDelta_CleanMergeAppliesTheirsOnlyChanges is the load-bearing fake
// merge: theirs' add and delete apply, theirs' edit that ours made
// identically is kept (not re-applied), and ours' own edit is untouched. It
// also pins the two-leg compare shape (views for committed sides, direct
// mounts for the active side) and the blob lease/pin hygiene across both legs.
func TestApplyDelta_CleanMergeAppliesTheirsOnlyChanges(t *testing.T) {
	sb, snap, tasks, fl, fds, base := deltaTestSandbox(t)
	ctx := context.Background()

	// Theirs (base → theirs layer): an add, an edit ours made identically,
	// and a delete.
	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/theirs-new.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("T\n")},
		tarEntry{name: "workspace/both-same.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("same\n")},
		tarEntry{name: "workspace/.wh.gone.txt", mode: 0, typeflag: tar.TypeReg},
	))
	// Ours (base → active): the identical edit, plus our own.
	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/both-same.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("same\n")},
		tarEntry{name: "workspace/ours-only.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("O\n")},
	))

	before := len(tasks.allReqs)
	if err := sb.ApplyDelta(ctx, deltaState(base)); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	execs := tasks.allReqs[before:]

	// The add applied with its blob content; the kept entries did not.
	writes := decodeDeltaWrites(t, execs)
	if got := writes["/workspace/theirs-new.txt"]; got != "T\n" {
		t.Errorf("theirs-new.txt written = %q, want %q", got, "T\n")
	}
	if len(writes) != 1 {
		t.Errorf("writes = %v, want only theirs-new.txt (identical and ours-only edits must not be re-applied)", writes)
	}
	// The delete applied, plus the write's preceding rm (every delta write
	// rm -rf's its path first — the blob cannot distinguish add from
	// modify, so the base's type under the path is unknown).
	if paths := scriptPaths(execs, deltaRemoveScript); fmt.Sprint(paths) != "[/workspace/gone.txt /workspace/theirs-new.txt]" {
		t.Errorf("rm execs = %v, want [gone.txt theirs-new.txt]", paths)
	}

	// Two compares: theirs (base→theirs) mounts BOTH sides through views
	// (both committed); ours (base→active) views the base and mounts the
	// active layer directly.
	calls := fds.compareCalls()
	if len(calls) != 2 {
		t.Fatalf("Compare calls = %d, want 2", len(calls))
	}
	sb.mu.Lock()
	active := sb.activeKey
	sb.mu.Unlock()
	for _, target := range []string{calls[0].lower, calls[0].upper} {
		if !strings.Contains(target, "/diffview/") {
			t.Errorf("theirs side mounted at %q, want a diffview (committed layer)", target)
		}
	}
	if !strings.Contains(calls[1].lower, "/diffview/") {
		t.Errorf("ours lower mounted at %q, want a diffview", calls[1].lower)
	}
	if calls[1].upper != "/mnt/"+active {
		t.Errorf("ours upper mounted at %q, want the active layer /mnt/%s directly", calls[1].upper, active)
	}

	// Hygiene: both per-call diff leases deleted, every view removed, both
	// content pins added and dropped.
	for _, id := range diffLeaseIDs(fl) {
		found := false
		fl.mu.Lock()
		for _, d := range fl.deleted {
			if d == id {
				found = true
			}
		}
		fl.mu.Unlock()
		if !found {
			t.Errorf("diff lease %s was not deleted after ApplyDelta", id)
		}
	}
	if keys := viewKeys(snap); len(keys) != 0 {
		t.Errorf("diff views leaked: %v", keys)
	}
	fl.mu.Lock()
	var addedContent, removedContent int
	for _, p := range fl.added {
		if p.res.Type == "content" {
			addedContent++
		}
	}
	for _, p := range fl.removed {
		if p.res.Type == "content" {
			removedContent++
		}
	}
	fl.mu.Unlock()
	if addedContent != 2 || removedContent != 2 {
		t.Errorf("content pins: %d added, %d removed; want 2 and 2 (one per leg)", addedContent, removedContent)
	}
}

// TestApplyDelta_ConflictRefusesWithZeroMutations pins the atomic refusal:
// divergent edits to the same file produce ErrDeltaConflict naming the path,
// and not a single mutation exec runs.
func TestApplyDelta_ConflictRefusesWithZeroMutations(t *testing.T) {
	sb, _, tasks, _, fds, base := deltaTestSandbox(t)
	ctx := context.Background()

	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/f.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("their edit\n")},
	))
	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/f.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("our edit\n")},
	))

	before := len(tasks.allReqs)
	err := sb.ApplyDelta(ctx, deltaState(base))
	if !errors.Is(err, ErrDeltaConflict) {
		t.Fatalf("err = %v, want ErrDeltaConflict", err)
	}
	if !strings.Contains(err.Error(), "f.txt") {
		t.Errorf("error %q does not name the conflicting path", err)
	}
	if len(tasks.allReqs) != before {
		t.Errorf("refusal ran %d execs, want 0 (the workspace must be untouched)", len(tasks.allReqs)-before)
	}
}

// TestApplyDelta_NoopMergesRunNoMutations pins the two no-op classes: an
// empty theirs, and both sides having made identical edits.
func TestApplyDelta_NoopMergesRunNoMutations(t *testing.T) {
	empty := buildTar(t, tarEntry{name: "workspace/", mode: 0o755, typeflag: tar.TypeDir})
	same := buildTar(t, tarEntry{name: "workspace/f.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("same\n")})

	tests := []struct {
		name         string
		theirs, ours []byte
	}{
		{"empty theirs", empty, empty},
		{"identical edits", same, same},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb, _, tasks, _, fds, base := deltaTestSandbox(t)
			ctx := context.Background()
			fds.queueBlob(tt.theirs)
			fds.queueBlob(tt.ours)

			before := len(tasks.allReqs)
			if err := sb.ApplyDelta(ctx, deltaState(base)); err != nil {
				t.Fatalf("ApplyDelta: %v", err)
			}
			if len(tasks.allReqs) != before {
				t.Errorf("no-op merge ran %d execs, want 0", len(tasks.allReqs)-before)
			}
		})
	}
}

// TestApplyDelta_TypeReplacementOrdersOps pins the application order for a
// file→directory replacement arriving as a dir entry plus children: the
// conditional mkdir script runs, then the child write — and for a deleted
// sibling, the rm runs before both (removes precede creates).
func TestApplyDelta_TypeReplacementOrdersOps(t *testing.T) {
	sb, _, tasks, _, fds, base := deltaTestSandbox(t)
	ctx := context.Background()

	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/.wh.oldfile", mode: 0, typeflag: tar.TypeReg},
		tarEntry{name: "workspace/p/", mode: 0o755, typeflag: tar.TypeDir},
		tarEntry{name: "workspace/p/f.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("F\n")},
	))
	fds.queueBlob(buildTar(t, tarEntry{name: "workspace/", mode: 0o755, typeflag: tar.TypeDir}))

	before := len(tasks.allReqs)
	if err := sb.ApplyDelta(ctx, deltaState(base)); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	execs := tasks.allReqs[before:]

	// Order: rm oldfile, mkdir p, then the child write with its own
	// preceding rm (every delta write rm's its path first).
	var order []string
	for _, r := range execs {
		if r.Command != "sh" || len(r.Args) < 4 {
			continue
		}
		switch {
		case r.Args[1] == deltaRemoveScript:
			order = append(order, "rm:"+r.Args[3])
		case r.Args[1] == deltaMkdirScript:
			order = append(order, "mkdir:"+r.Args[3])
		case strings.Contains(r.Args[1], "base64 -d"):
			order = append(order, "write:"+r.Args[3])
		}
	}
	want := "[rm:/workspace/oldfile mkdir:/workspace/p rm:/workspace/p/f.txt write:/workspace/p/f.txt]"
	if fmt.Sprint(order) != want {
		t.Errorf("op order = %v, want %s", order, want)
	}
}

// TestApplyDelta_SymlinkFromBlobTarget pins symlink application: the target
// comes from theirs' blob (Linkname), applied via rm-then-ln.
func TestApplyDelta_SymlinkFromBlobTarget(t *testing.T) {
	sb, _, tasks, _, fds, base := deltaTestSandbox(t)
	ctx := context.Background()

	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/link", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "theirs-target"},
	))
	fds.queueBlob(buildTar(t, tarEntry{name: "workspace/", mode: 0o755, typeflag: tar.TypeDir}))

	before := len(tasks.allReqs)
	if err := sb.ApplyDelta(ctx, deltaState(base)); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	var found bool
	for _, r := range tasks.allReqs[before:] {
		if r.Command == "sh" && len(r.Args) >= 5 && r.Args[1] == deltaSymlinkScript {
			found = true
			if r.Args[3] != "theirs-target" || r.Args[4] != "/workspace/link" {
				t.Errorf("ln args = %q %q, want target then link path", r.Args[3], r.Args[4])
			}
		}
	}
	if !found {
		t.Error("no ln -s exec for the symlink entry")
	}
}

// TestApplyDelta_SpecialFileRefusesBeforeMutation pins the applicability
// refusal: a FIFO in theirs fails the whole call before any mutation exec.
func TestApplyDelta_SpecialFileRefusesBeforeMutation(t *testing.T) {
	sb, _, tasks, _, fds, base := deltaTestSandbox(t)
	ctx := context.Background()

	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/ok.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("fine\n")},
		tarEntry{name: "workspace/pipe", mode: 0o644, typeflag: tar.TypeFifo},
	))
	fds.queueBlob(buildTar(t, tarEntry{name: "workspace/", mode: 0o755, typeflag: tar.TypeDir}))

	before := len(tasks.allReqs)
	err := sb.ApplyDelta(ctx, deltaState(base))
	if err == nil || !strings.Contains(err.Error(), "cannot apply special file pipe") {
		t.Fatalf("err = %v, want a cannot-apply-special-file refusal", err)
	}
	if len(tasks.allReqs) != before {
		t.Errorf("refusal ran %d execs, want 0", len(tasks.allReqs)-before)
	}
}

// TestDeltaMkdirScript runs the real mkdir script under the host's /bin/sh
// against a real filesystem — the semantics the fake cannot model: a
// symlink-to-directory must be replaced rather than written through (a plain
// `[ ! -d ]` guard follows the link and would chmod the TARGET, possibly
// outside the workspace), a real directory with kept children must survive,
// and the symbolic pre-clear must make the numeric mode absolute (short
// numeric modes preserve a directory's setgid under preservation-quirk chmod
// implementations — verified against uutils and BusyBox).
func TestDeltaMkdirScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	run := func(dir, mode string) {
		t.Helper()
		cmd := exec.Command(sh, "-c", deltaMkdirScript, "croft", dir, mode)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mkdir script on %s: %v\n%s", dir, err, out)
		}
	}

	t.Run("replaces a symlink-to-directory without touching the target", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		link := filepath.Join(root, "d")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o755|fs.ModeSetgid); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		run(link, "0750")

		info, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("d is %v, want a real directory (the symlink must be replaced)", info.Mode())
		}
		if info.Mode().Perm() != 0o750 {
			t.Errorf("d perm = %04o, want 0750", info.Mode().Perm())
		}
		ti, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if ti.Mode()&fs.ModeSetgid == 0 || ti.Mode().Perm() != 0o755 {
			t.Errorf("target mode = %v, want untouched setgid 0755 (chmod must not write through the link)", ti.Mode())
		}
	})

	t.Run("preserves a real directory and its children, clears special bits", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "e")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755|fs.ModeSetgid); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(dir, "keep.txt")
		if err := os.WriteFile(child, []byte("kept\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		run(dir, "0755")

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("e is %v, want a surviving directory", info.Mode())
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("e perm = %04o, want 0755", info.Mode().Perm())
		}
		if info.Mode()&fs.ModeSetgid != 0 {
			t.Errorf("e mode = %v, want setgid cleared (the numeric mode is absolute)", info.Mode())
		}
		if got, err := os.ReadFile(child); err != nil || string(got) != "kept\n" {
			t.Errorf("kept child = %q, %v; want %q, nil", got, err, "kept\n")
		}
	})

	t.Run("creates an absent directory with the exact mode", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "n", "deep")
		run(dir, "0700")

		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("n/deep = %v, want a directory with perm 0700", info.Mode())
		}
	})
}

// TestApplyDelta_OpaqueDirExpansionStaysSorted is the regression for
// unsorted merge inputs: the synthesized opaque-dir deletes are appended
// after the parser's sorted entries, and an out-of-order delete would slip
// past the two-pointer merge — applied over ours' change instead of
// conflicting with it. Theirs' opaque dir hides the base's d/a.txt (which
// ours modified); the correct merge is a modify/delete conflict on d/a.txt,
// while the unsorted walk would silently apply the delete.
func TestApplyDelta_OpaqueDirExpansionStaysSorted(t *testing.T) {
	sb, _, tasks, _, fds, base := deltaTestSandbox(t)
	ctx := context.Background()

	// Theirs: an opaque marker on d plus one new file in it. The base's d
	// held a.txt and z.txt, so resolving the marker synthesizes a delete
	// for d/a.txt — appended AFTER the parser's d/z.txt entry unless the
	// list is re-sorted.
	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/d/.wh..wh..opq", mode: 0, typeflag: tar.TypeReg},
		tarEntry{name: "workspace/d/z.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("Z\n")},
	))
	tasks.lsDirs["/workspace/d"] = []string{"a.txt", "z.txt"}
	// Ours: a modification to one of the hidden files.
	fds.queueBlob(buildTar(t,
		tarEntry{name: "workspace/d/a.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("ours\n")},
	))

	before := len(tasks.allReqs)
	err := sb.ApplyDelta(ctx, deltaState(base))
	if !errors.Is(err, ErrDeltaConflict) {
		t.Fatalf("err = %v, want ErrDeltaConflict (delete of d/a.txt vs ours' modify)", err)
	}
	if !strings.Contains(err.Error(), "d/a.txt") {
		t.Errorf("conflict error %q does not name d/a.txt", err)
	}
	// No mutation ran (the opaque resolution's ls is reader-task work, not
	// a mutation): nothing removed, nothing written.
	execs := tasks.allReqs[before:]
	if paths := scriptPaths(execs, deltaRemoveScript); len(paths) != 0 {
		t.Errorf("refusal ran rm on %v, want none", paths)
	}
	if writes := decodeDeltaWrites(t, execs); len(writes) != 0 {
		t.Errorf("refusal wrote %v, want none", writes)
	}
}
