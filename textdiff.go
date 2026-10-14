package croft

import (
	"bytes"
	"fmt"
	"io/fs"
	"strings"
)

// This file is the stdlib-only text diff engine: an LCS-based edit script,
// hunk grouping with context, and a unified-diff renderer. No external
// dependencies — go-diff is deliberately not added.

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

// diffLines computes the edit script between a and b using LCS dynamic
// programming. For small files (the common case for code changes), O(N*M)
// is fine; the Myers O(ND) algorithm would be faster for large files with
// few changes, but the DP approach is simpler and correct.
func diffLines(a, b []string) []editStep {
	m, n := len(a), len(b)
	if m == 0 && n == 0 {
		return nil
	}

	// dp[i][j] = LCS length of a[:i] and b[:j].
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack from (m, n) to (0, 0), building the script backwards.
	var steps []editStep
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			steps = append(steps, editStep{op: opMatch, oldLine: i - 1, newLine: j - 1, line: a[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			steps = append(steps, editStep{op: opInsert, oldLine: -1, newLine: j - 1, line: b[j-1]})
			j--
		default:
			steps = append(steps, editStep{op: opDelete, oldLine: i - 1, newLine: -1, line: a[i-1]})
			i--
		}
	}

	// Reverse: the backtrack built the script in reverse order.
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
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
func computeHunks(oldLines, newLines []string, groups []changeGroup) []hunk {
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

		var hl []hunkLine
		oi, ni := oldStart, newStart

		// Context before the change (matches).
		for oi < g.oldStart && ni < g.newStart {
			hl = append(hl, hunkLine{prefix: ' ', line: oldLines[oi]})
			oi++
			ni++
		}
		// Deleted lines.
		for oi < g.oldEnd {
			hl = append(hl, hunkLine{prefix: '-', line: oldLines[oi]})
			oi++
		}
		// Added lines.
		for ni < g.newEnd {
			hl = append(hl, hunkLine{prefix: '+', line: newLines[ni]})
			ni++
		}
		// Context after the change (matches).
		for oi < oldEnd && ni < newEnd {
			hl = append(hl, hunkLine{prefix: ' ', line: oldLines[oi]})
			oi++
			ni++
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
	hunks := computeHunks(oldLines, newLines, groups)
	if len(hunks) == 0 {
		return ""
	}

	var out strings.Builder
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
	for _, h := range hunks {
		out.WriteString(hunkHeader(h))
		out.WriteByte('\n')
		for _, hl := range h.lines {
			out.WriteByte(hl.prefix)
			out.WriteString(hl.line)
			if !strings.HasSuffix(hl.line, "\n") {
				out.WriteByte('\n')
			}
		}
	}
	return out.String()
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

// formatMode renders an fs.FileMode as a 4-digit octal string (e.g. "0644"),
// matching the `old mode`/`new mode` lines in unified diffs.
func formatMode(m fs.FileMode) string {
	return fmt.Sprintf("%04o", m.Perm())
}
