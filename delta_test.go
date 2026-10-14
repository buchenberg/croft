package croft

import (
	"fmt"
	"io/fs"
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
