package croft

import (
	"bytes"
	"sort"
)

// This file is the snapshot-native delta: a tree-level three-way merge of two
// change sets against their common base layer, replacing the git-era
// ApplyDelta's `git merge-tree` over a carried binary patch. Nothing is
// carried in the state — the merge is computed from two daemon-side diffs
// (base → theirs, base → ours) at call time.

// mergeChanges performs the tree-level merge: theirs (base → the incoming
// state) against ours (base → the current workspace). Both change lists must
// be sorted by Path, which the tar parser guarantees. The content maps carry
// each side's post-images from its own diff blob, so the identical-edit check
// compares bytes without touching either workspace; a missing map entry means
// "no content" (the parser omits empty-file bodies, directories, and special
// files), and bytes.Equal treats nil and empty alike — two empty files are
// identical.
//
// Per-path rules:
//
//   - only theirs changed → take theirs: the entry joins apply.
//   - only ours changed → keep ours: nothing to do, no entry.
//   - both changed → delete+delete keeps; non-deletes with identical bytes
//     AND identical mode keep; every other combination (modify/delete,
//     delete/modify, divergent edits, mode disagreement, type change) is a
//     conflict.
//
// One rule reaches beyond single paths: a delete on one side conflicts with
// any non-delete change strictly underneath it on the other (theirs removes
// dir/ while ours wrote dir/f, or vice versa). The per-path walk cannot see
// it — the paths differ — but the intents contradict, and silently applying
// the delete would drop the kept child (or resurrect the deleted subtree on
// the child's write). Deleted directories carry per-child whiteouts in the
// same side's change set, so this rule fires only when the child appeared
// after the base — exactly the divergent-intent case.
//
// conflicts carries theirs-side entries: the incoming paths the merge could
// not reconcile, which is what the ErrDeltaConflict error names. It is sorted
// by path and deduplicated; apply keeps theirs' order minus any path demoted
// to a conflict. The function is pure — callers compute both change sets and
// hold all content before any mutation, so a non-empty conflicts list means
// the workspace was never touched.
func mergeChanges(theirs, ours []Change, theirsContent, oursContent map[string][]byte) (apply []Change, conflicts []Change) {
	// Per-path walk over the two sorted lists.
	conflicted := make(map[string]bool)
	addConflict := func(c Change) {
		if !conflicted[c.Path] {
			conflicted[c.Path] = true
			conflicts = append(conflicts, c)
		}
	}

	ti, oi := 0, 0
	for ti < len(theirs) && oi < len(ours) {
		t, o := theirs[ti], ours[oi]
		switch {
		case t.Path < o.Path:
			apply = append(apply, t)
			ti++
		case o.Path < t.Path:
			oi++ // keep ours
		default:
			ti++
			oi++
			if !identicalChanges(t, o, theirsContent, oursContent) {
				addConflict(t)
			} // else keep: both sides made the same change
		}
	}
	apply = append(apply, theirs[ti:]...)

	// Delete-vs-underneath collisions, in both directions.
	for _, d := range apply {
		if d.Kind != ChangeDelete {
			continue
		}
		forEachUnder(ours, d.Path, func(o Change) bool {
			if o.Kind != ChangeDelete {
				addConflict(d)
				return false
			}
			return true
		})
	}
	for _, d := range ours {
		if d.Kind != ChangeDelete {
			continue
		}
		forEachUnder(theirs, d.Path, func(t Change) bool {
			if t.Kind != ChangeDelete {
				addConflict(t)
			}
			return true
		})
	}

	if len(conflicts) > 0 {
		// Conflicted paths never join the apply set: refusal is all-or-
		// nothing at the caller, but the algebra stays honest in isolation.
		kept := apply[:0]
		for _, a := range apply {
			if !conflicted[a.Path] {
				kept = append(kept, a)
			}
		}
		apply = kept
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	}
	return apply, conflicts
}

// identicalChanges reports whether both sides made the same change to the
// same path, so the merge can keep it instead of conflicting. Two deletes
// always match — the path is gone from both trees, and delta change sets
// carry no classified metadata on deletes to disagree about. Non-deletes
// match on identical mode (the full fs.FileMode, so type changes and
// special bits count) and identical bytes.
func identicalChanges(t, o Change, theirsContent, oursContent map[string][]byte) bool {
	tDel, oDel := t.Kind == ChangeDelete, o.Kind == ChangeDelete
	if tDel || oDel {
		return tDel && oDel
	}
	if t.Mode != o.Mode || t.IsDir != o.IsDir {
		return false
	}
	return bytes.Equal(theirsContent[t.Path], oursContent[o.Path])
}

// forEachUnder calls fn for every entry whose path lies strictly under dir
// (prefix dir+"/"), in sorted order, stopping early when fn returns false.
// The list must be sorted by Path: the range is found by binary search and
// bounded above by dir+"0", since '/' (0x2F) sorts below every character
// that could follow a completed path segment ('0' is 0x30).
func forEachUnder(list []Change, dir string, fn func(Change) bool) {
	prefix := dir + "/"
	hi := dir + "0"
	for i := sort.Search(len(list), func(i int) bool { return list[i].Path >= prefix }); i < len(list) && list[i].Path < hi; i++ {
		if !fn(list[i]) {
			return
		}
	}
}
