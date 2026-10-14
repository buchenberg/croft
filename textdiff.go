package croft

import (
	"bytes"
	"fmt"
	"io/fs"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// This file is the text diff engine: a Myers O(ND) edit script (via
// github.com/sergi/go-diff), hunk grouping with context, and a unified-diff
// renderer.

// contextLines is the number of unchanged lines shown around each change
// group in a unified diff hunk.
const contextLines = 3

// diffOp classifies one edit step.
type diffOp int

const (
	opMatch  diffOp = iota // line is the same in both
	opDelete               // line is in old but not new
	opInsert               // line is in new but not old
)

// editStep is one step in the edit script between two line sequences.
type editStep struct {
	op      diffOp
	oldLine int    // 0-based index into old, -1 for insert
	newLine int    // 0-based index into new, -1 for delete
	line    string // the line content (with trailing newline, if any)
}

// diffLines computes the edit script between a and b using the Myers O(ND)
// algorithm (via github.com/sergi/go-diff). The lines are joined, encoded
// line-to-char for O(ND) performance on the encoded text, diffed, and
// decoded back to line-level diffs. The result is a shortest edit script:
// a sequence of match/delete/insert steps.
func diffLines(a, b []string) []editStep {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}

	dmp := diffmatchpatch.New()

	// Join lines (each already carries its trailing newline, if any).
	textA := strings.Join(a, "")
	textB := strings.Join(b, "")

	// Line-to-char encoding: each unique line becomes one rune, so the
	// char-level Myers diff runs at line granularity in O(ND) where N and D
	// are line counts, not byte counts.
	charsA, charsB, lineArray := dmp.DiffLinesToChars(textA, textB)
	diffs := dmp.DiffMain(charsA, charsB, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

	// Convert to edit steps, tracking line indices.
	var steps []editStep
	oldIdx, newIdx := 0, 0
	for _, d := range diffs {
		for _, line := range splitLines([]byte(d.Text)) {
			switch d.Type {
			case diffmatchpatch.DiffEqual:
				steps = append(steps, editStep{op: opMatch, oldLine: oldIdx, newLine: newIdx, line: line})
				oldIdx++
				newIdx++
			case diffmatchpatch.DiffDelete:
				steps = append(steps, editStep{op: opDelete, oldLine: oldIdx, newLine: -1, line: line})
				oldIdx++
			case diffmatchpatch.DiffInsert:
				steps = append(steps, editStep{op: opInsert, oldLine: -1, newLine: newIdx, line: line})
				newIdx++
			}
		}
	}
	return steps
}

// changeGroup is a maximal run of consecutive insertions/deletions, with the
// line ranges they span in old and new.
type changeGroup struct {
	oldStart, oldEnd int // [oldStart, oldEnd) in old
	newStart, newEnd int // [newStart, newEnd) in new
}

// computeChangeGroups converts an edit script into change groups: maximal
// runs of consecutive non-match steps.
func computeChangeGroups(steps []editStep) []changeGroup {
	var groups []changeGroup
	oldIdx, newIdx := 0, 0
	inGroup := false

	for _, step := range steps {
		switch step.op {
		case opMatch:
			oldIdx++
			newIdx++
			inGroup = false
		case opDelete:
			if !inGroup {
				groups = append(groups, changeGroup{oldStart: oldIdx, oldEnd: oldIdx, newStart: newIdx, newEnd: newIdx})
				inGroup = true
			}
			groups[len(groups)-1].oldEnd++
			oldIdx++
		case opInsert:
			if !inGroup {
				groups = append(groups, changeGroup{oldStart: oldIdx, oldEnd: oldIdx, newStart: newIdx, newEnd: newIdx})
				inGroup = true
			}
			groups[len(groups)-1].newEnd++
			newIdx++
		}
	}
	return groups
}

// mergeGroups merges change groups whose context ranges would overlap: two
// groups separated by <= 2*contextLines unchanged lines belong to the same
// hunk.
func mergeGroups(groups []changeGroup) []changeGroup {
	if len(groups) <= 1 {
		return groups
	}
	var merged []changeGroup
	current := groups[0]
	for _, g := range groups[1:] {
		gap := g.oldStart - current.oldEnd
		if gap <= 2*contextLines {
			current.oldEnd = g.oldEnd
			current.newEnd = g.newEnd
		} else {
			merged = append(merged, current)
			current = g
		}
	}
	return append(merged, current)
}

// hunkLine is one line in a unified diff hunk: a prefix (' ', '-', '+') and
// the line content.
type hunkLine struct {
	prefix byte
	line   string
}

// hunk is one unified-diff hunk.
type hunk struct {
	oldStart, oldCount int // 0-based start; count of old-file lines in the hunk
	newStart, newCount int // 0-based start; count of new-file lines in the hunk
	lines              []hunkLine
}

// computeHunks converts change groups into unified-diff hunks with context.
// It walks the original edit steps (not just the group boundaries) so that
// unchanged lines INSIDE a merged group — between two nearby changes that
// mergeGroups joined — render as context (' '), not as deletions+insertions.
// Without this, merged hunks are non-minimal and diverge from `git diff`.
func computeHunks(oldLines, newLines []string, groups []changeGroup, steps []editStep) []hunk {
	var hunks []hunk
	for _, g := range groups {
		oldStart := g.oldStart - contextLines
		if oldStart < 0 {
			oldStart = 0
		}
		newStart := g.newStart - contextLines
		if newStart < 0 {
			newStart = 0
		}
		oldEnd := g.oldEnd + contextLines
		if oldEnd > len(oldLines) {
			oldEnd = len(oldLines)
		}
		newEnd := g.newEnd + contextLines
		if newEnd > len(newLines) {
			newEnd = len(newLines)
		}

		// Walk the edit steps, rendering those within this hunk's span.
		// Matches render as context, deletes as '-', inserts as '+'.
		// Interior matches within a merged group are preserved as context.
		var hl []hunkLine
		for _, step := range steps {
			inOld := step.oldLine >= oldStart && step.oldLine < oldEnd
			inNew := step.newLine >= newStart && step.newLine < newEnd
			if !inOld && !inNew {
				continue
			}
			switch step.op {
			case opMatch:
				hl = append(hl, hunkLine{prefix: ' ', line: step.line})
			case opDelete:
				hl = append(hl, hunkLine{prefix: '-', line: step.line})
			case opInsert:
				hl = append(hl, hunkLine{prefix: '+', line: step.line})
			}
		}

		hunks = append(hunks, hunk{
			oldStart: oldStart,
			oldCount: oldEnd - oldStart,
			newStart: newStart,
			newCount: newEnd - newStart,
			lines:    hl,
		})
	}
	return hunks
}

// hunkHeader renders the @@ -l,c +l,c @@ line. Line numbers are 1-based; a
// count of 0 means the start is the line before the hunk (0 at file start).
// A count of 1 is rendered without the comma (the unified-diff convention).
func hunkHeader(h hunk) string {
	oldStart := h.oldStart + 1
	if h.oldCount == 0 {
		oldStart = h.oldStart
	}
	newStart := h.newStart + 1
	if h.newCount == 0 {
		newStart = h.newStart
	}
	return fmt.Sprintf("@@ -%s +%s @@", rangeStr(oldStart, h.oldCount), rangeStr(newStart, h.newCount))
}

// rangeStr renders a line range as "start" (count 1) or "start,count".
func rangeStr(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// splitLines splits content into lines, keeping the trailing newline on each
// line. An empty input yields nil.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// isBinary reports whether content looks binary: a NUL byte in the first
// 8 KiB (the same heuristic git uses).
func isBinary(content []byte) bool {
	n := len(content)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(content[:n], 0) >= 0
}

// renderUnified renders a unified diff between oldContent and newContent.
// isAdd and isDelete select the /dev/null side: an add has no old side, a
// delete has no new side. The path is workspace-relative.
func renderUnified(path string, oldContent, newContent []byte, isAdd, isDelete bool) string {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)
	steps := diffLines(oldLines, newLines)
	groups := mergeGroups(computeChangeGroups(steps))
	hunks := computeHunks(oldLines, newLines, groups, steps)

	if len(hunks) == 0 {
		// No line-level changes. For an add or delete of an empty file, the
		// headers still appear with a 0-line hunk; for identical content,
		// nothing is emitted (the caller should not have called us).
		if !isAdd && !isDelete {
			return ""
		}
		if len(oldLines) > 0 || len(newLines) > 0 {
			return "" // changes but no hunks: shouldn't happen
		}
		var out strings.Builder
		writeUnifiedHeaders(&out, path, isAdd, isDelete)
		out.WriteString("@@ -0,0 +0,0 @@\n")
		return out.String()
	}

	var out strings.Builder
	writeUnifiedHeaders(&out, path, isAdd, isDelete)
	for _, h := range hunks {
		out.WriteString(hunkHeader(h))
		out.WriteByte('\n')
		for _, hl := range h.lines {
			out.WriteByte(hl.prefix)
			out.WriteString(hl.line)
			if !strings.HasSuffix(hl.line, "\n") {
				// The unified-diff convention: a line without a trailing
				// newline is followed by the marker line.
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
	}
	return out.String()
}

// writeUnifiedHeaders writes the ---/+++ header pair.
func writeUnifiedHeaders(out *strings.Builder, path string, isAdd, isDelete bool) {
	if isAdd {
		out.WriteString("--- /dev/null\n")
	} else {
		out.WriteString("--- a/" + path + "\n")
	}
	if isDelete {
		out.WriteString("+++ /dev/null\n")
	} else {
		out.WriteString("+++ b/" + path + "\n")
	}
}

// parseSymbolicMode converts an ls -l symbolic mode string (e.g.
// "-rw-r--r--" or "drwxr-xr-x") to an fs.FileMode. It handles the common
// cases: file type, permission bits, setuid/setgid/sticky. ACLs and extended
// attributes are not represented.
func parseSymbolicMode(s string) fs.FileMode {
	if len(s) < 10 {
		return 0
	}
	var mode fs.FileMode
	switch s[0] {
	case 'd':
		mode |= fs.ModeDir
	case 'l':
		mode |= fs.ModeSymlink
	case 'c':
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 'b':
		mode |= fs.ModeDevice
	case 'p':
		mode |= fs.ModeNamedPipe
	case 's':
		mode |= fs.ModeSocket
	}
	// Owner: positions 1-3.
	if s[1] == 'r' {
		mode |= 0o400
	}
	if s[2] == 'w' {
		mode |= 0o200
	}
	switch s[3] {
	case 'x':
		mode |= 0o100
	case 's':
		mode |= 0o100 | fs.ModeSetuid
	case 'S':
		mode |= fs.ModeSetuid
	}
	// Group: positions 4-6.
	if s[4] == 'r' {
		mode |= 0o040
	}
	if s[5] == 'w' {
		mode |= 0o020
	}
	switch s[6] {
	case 'x':
		mode |= 0o010
	case 's':
		mode |= 0o010 | fs.ModeSetgid
	case 'S':
		mode |= fs.ModeSetgid
	}
	// Other: positions 7-9.
	if s[7] == 'r' {
		mode |= 0o004
	}
	if s[8] == 'w' {
		mode |= 0o002
	}
	switch s[9] {
	case 'x':
		mode |= 0o001
	case 't':
		mode |= 0o001 | fs.ModeSticky
	case 'T':
		mode |= fs.ModeSticky
	}
	return mode
}

// effectivePerm returns the permission bits including setuid/setgid/sticky,
// mapped onto Unix octal bits (04000/02000/01000). fs.FileMode.Perm() masks
// to 0777 and drops these; mode-only detection must compare them.
func effectivePerm(m fs.FileMode) fs.FileMode {
	perm := m.Perm()
	if m&fs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		perm |= 0o1000
	}
	return perm
}

// formatMode renders an fs.FileMode as a 4-digit octal string (e.g. "0644",
// "4755" for setuid+0755), matching the `old mode`/`new mode` lines in
// unified diffs. Special bits are included.
func formatMode(m fs.FileMode) string {
	return fmt.Sprintf("%04o", effectivePerm(m))
}
