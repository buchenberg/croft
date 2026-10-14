package croft

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestDiffLines_Identical(t *testing.T) {
	a := []string{"one\n", "two\n", "three\n"}
	steps := diffLines(a, a)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3 (all matches)", len(steps))
	}
	for i, s := range steps {
		if s.op != opMatch {
			t.Errorf("step[%d].op = %v, want match", i, s.op)
		}
	}
}

func TestDiffLines_Empty(t *testing.T) {
	if steps := diffLines(nil, nil); len(steps) != 0 {
		t.Errorf("diffLines(nil, nil) = %v, want empty", steps)
	}
	if steps := diffLines(nil, []string{"a\n"}); len(steps) != 1 || steps[0].op != opInsert {
		t.Errorf("diffLines(nil, [a]) = %v, want one insert", steps)
	}
	if steps := diffLines([]string{"a\n"}, nil); len(steps) != 1 || steps[0].op != opDelete {
		t.Errorf("diffLines([a], nil) = %v, want one delete", steps)
	}
}

func TestDiffLines_Modify(t *testing.T) {
	old := []string{"a\n", "b\n", "c\n"}
	new := []string{"a\n", "X\n", "c\n"}
	steps := diffLines(old, new)

	// Expect: match a, delete b, insert X, match c.
	want := []diffOp{opMatch, opDelete, opInsert, opMatch}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %d steps", steps, len(want))
	}
	for i, w := range want {
		if steps[i].op != w {
			t.Errorf("step[%d].op = %v, want %v", i, steps[i].op, w)
		}
	}
}

func TestComputeHunks_SingleChange(t *testing.T) {
	old := []string{"a\n", "b\n", "c\n", "d\n", "e\n", "f\n", "g\n"}
	new := []string{"a\n", "b\n", "c\n", "X\n", "e\n", "f\n", "g\n"}
	steps := diffLines(old, new)
	groups := mergeGroups(computeChangeGroups(steps))
	hunks := computeHunks(old, new, groups)

	if len(hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(hunks))
	}
	h := hunks[0]
	// Change at old index 3 (d), new index 3 (X). Context: 3 before, 3 after.
	// oldStart = max(0, 3-3) = 0, oldEnd = min(7, 4+3) = 7 → oldCount = 7.
	// newStart = 0, newEnd = 7 → newCount = 7.
	if h.oldStart != 0 || h.oldCount != 7 || h.newStart != 0 || h.newCount != 7 {
		t.Errorf("hunk ranges = old[%d,%d) new[%d,%d), want old[0,7) new[0,7)",
			h.oldStart, h.oldStart+h.oldCount, h.newStart, h.newStart+h.newCount)
	}
	// Lines: a b c (context) -d +X e f g (context).
	wantPrefixes := []byte{' ', ' ', ' ', '-', '+', ' ', ' ', ' '}
	if len(h.lines) != len(wantPrefixes) {
		t.Fatalf("hunk lines = %d, want %d", len(h.lines), len(wantPrefixes))
	}
	for i, w := range wantPrefixes {
		if h.lines[i].prefix != w {
			t.Errorf("line[%d].prefix = %c, want %c", i, h.lines[i].prefix, w)
		}
	}
}

func TestComputeHunks_TwoSeparateChanges(t *testing.T) {
	// Changes far apart (> 2*contextLines) → two hunks.
	old := []string{"a\n", "b\n", "c\n", "d\n", "e\n", "f\n", "g\n", "h\n", "i\n", "j\n"}
	new := []string{"A\n", "b\n", "c\n", "d\n", "e\n", "f\n", "g\n", "h\n", "i\n", "J\n"}
	steps := diffLines(old, new)
	groups := mergeGroups(computeChangeGroups(steps))
	hunks := computeHunks(old, new, groups)

	if len(hunks) != 2 {
		t.Fatalf("hunks = %d, want 2 (changes too far apart to merge)", len(hunks))
	}
}

func TestComputeHunks_CloseChangesMerge(t *testing.T) {
	// Changes within 2*contextLines → one hunk.
	old := []string{"a\n", "b\n", "c\n", "d\n", "e\n", "f\n", "g\n"}
	new := []string{"A\n", "b\n", "c\n", "d\n", "e\n", "f\n", "G\n"}
	steps := diffLines(old, new)
	groups := mergeGroups(computeChangeGroups(steps))
	hunks := computeHunks(old, new, groups)

	if len(hunks) != 1 {
		t.Fatalf("hunks = %d, want 1 (changes close enough to merge)", len(hunks))
	}
}

func TestHunkHeader(t *testing.T) {
	tests := []struct {
		h    hunk
		want string
	}{
		{hunk{oldStart: 0, oldCount: 7, newStart: 0, newCount: 7}, "@@ -1,7 +1,7 @@"},
		{hunk{oldStart: 4, oldCount: 3, newStart: 4, newCount: 4}, "@@ -5,3 +5,4 @@"},
		// Pure insertion: oldCount=0, start is the line before.
		{hunk{oldStart: 2, oldCount: 0, newStart: 2, newCount: 1}, "@@ -2,0 +3 @@"},
		// Pure deletion: newCount=0.
		{hunk{oldStart: 2, oldCount: 1, newStart: 2, newCount: 0}, "@@ -3 +2,0 @@"},
	}
	for _, tt := range tests {
		if got := hunkHeader(tt.h); got != tt.want {
			t.Errorf("hunkHeader(%+v) = %q, want %q", tt.h, got, tt.want)
		}
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		content []byte
		want    []string
	}{
		{nil, nil},
		{[]byte(""), nil},
		{[]byte("a\n"), []string{"a\n"}},
		{[]byte("a\nb\n"), []string{"a\n", "b\n"}},
		{[]byte("a\nb"), []string{"a\n", "b"}},
		{[]byte("\n"), []string{"\n"}},
	}
	for _, tt := range tests {
		got := splitLines(tt.content)
		if len(got) != len(tt.want) {
			t.Errorf("splitLines(%q) = %v, want %v", tt.content, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", tt.content, i, got[i], tt.want[i])
			}
		}
	}
}

func TestIsBinary(t *testing.T) {
	tests := []struct {
		content []byte
		want    bool
	}{
		{[]byte("hello\nworld\n"), false},
		{[]byte("hello\x00world"), true},
		{[]byte(""), false},
		{bytes.Repeat([]byte("a"), 8192), false},
		{append(bytes.Repeat([]byte("a"), 8191), 0), true},  // NUL at 8191
		{append(bytes.Repeat([]byte("a"), 8192), 0), false}, // NUL at 8192 (outside window)
	}
	for _, tt := range tests {
		if got := isBinary(tt.content); got != tt.want {
			t.Errorf("isBinary(%q...) = %v, want %v", tt.content[:min(20, len(tt.content))], got, tt.want)
		}
	}
}

func TestRenderUnified_Modify(t *testing.T) {
	old := []byte("line1\nline2\nline3\n")
	new := []byte("line1\nline2 changed\nline3\n")
	got := renderUnified("file.txt", old, new, false, false)

	want := `--- a/file.txt
+++ b/file.txt
@@ -1,3 +1,3 @@
 line1
-line2
+line2 changed
 line3
`
	if got != want {
		t.Errorf("renderUnified modify:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderUnified_Add(t *testing.T) {
	got := renderUnified("new.txt", nil, []byte("hello\n"), true, false)

	want := `--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hello
`
	if got != want {
		t.Errorf("renderUnified add:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderUnified_Delete(t *testing.T) {
	got := renderUnified("gone.txt", []byte("bye\n"), nil, false, true)

	want := `--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
`
	if got != want {
		t.Errorf("renderUnified delete:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderUnified_Identical(t *testing.T) {
	got := renderUnified("same.txt", []byte("a\n"), []byte("a\n"), false, false)
	if got != "" {
		t.Errorf("renderUnified identical = %q, want empty", got)
	}
}

func TestRenderUnified_EmptyToEmpty(t *testing.T) {
	got := renderUnified("empty.txt", nil, nil, true, false)
	if got != "" {
		t.Errorf("renderUnified empty-to-empty = %q, want empty", got)
	}
}

func TestRenderUnified_NoTrailingNewline(t *testing.T) {
	old := []byte("a\nb") // no trailing newline on b
	new := []byte("a\nc")
	got := renderUnified("f.txt", old, new, false, false)

	// The last line has no trailing newline; the renderer adds one.
	if !strings.Contains(got, "-b\n") {
		t.Errorf("renderUnified no-trailing-newline: missing '-b\\n' in %q", got)
	}
	if !strings.Contains(got, "+c\n") {
		t.Errorf("renderUnified no-trailing-newline: missing '+c\\n' in %q", got)
	}
}

func TestParseSymbolicMode(t *testing.T) {
	tests := []struct {
		symbolic string
		want     fs.FileMode
	}{
		{"-rw-r--r--", 0o644},
		{"-rw-------", 0o600},
		{"-rwxr-xr-x", 0o755},
		{"drwxr-xr-x", 0o755 | fs.ModeDir},
		{"lrwxrwxrwx", 0o777 | fs.ModeSymlink},
		{"-rwsr-xr-x", 0o755 | fs.ModeSetuid},
		{"-rwxr-sr-x", 0o755 | fs.ModeSetgid},
		{"-rwxr-xr-t", 0o755 | fs.ModeSticky},
		{"-rwSr--r--", 0o644 | fs.ModeSetuid},
		{"crw-rw-rw-", 0o666 | fs.ModeDevice | fs.ModeCharDevice},
		{"prw-r--r--", 0o644 | fs.ModeNamedPipe},
		{"srwxr-xr-x", 0o755 | fs.ModeSocket},
		{"----------", 0},
		{"too-short", 0},
	}
	for _, tt := range tests {
		if got := parseSymbolicMode(tt.symbolic); got != tt.want {
			t.Errorf("parseSymbolicMode(%q) = %o, want %o", tt.symbolic, got, tt.want)
		}
	}
}

func TestFormatMode(t *testing.T) {
	tests := []struct {
		mode fs.FileMode
		want string
	}{
		{0o644, "0644"},
		{0o600, "0600"},
		{0o755, "0755"},
		{0o000, "0000"},
		{0o777, "0777"},
		// Type bits are not rendered; only the permission bits.
		{0o755 | fs.ModeDir, "0755"},
		{0o777 | fs.ModeSymlink, "0777"},
	}
	for _, tt := range tests {
		if got := formatMode(tt.mode); got != tt.want {
			t.Errorf("formatMode(%o) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
